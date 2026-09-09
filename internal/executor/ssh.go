package executor

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// sshConnectTimeout 은 접속·재접속 시도 1회당 상한이다: TCP 연결 + 키 교환 + 인증을
// 전부 포함한다(SPEC §10.1 "시도 1회당 접속 타임아웃 10s"). 재시도(지수 백오프
// 1s→30s)는 이 Executor 를 재생성하는 machine 패키지(Phase 10)의 책임이다. [§7.1-8, §10.1]
const sshConnectTimeout = 10 * time.Second

// SSHConfig 는 NewSSH 의 재료다. [DESIGN §4.1]
type SSHConfig struct {
	Host                      string
	Port                      int
	User                      string
	KeyFile                   string
	KeyPassphrase             string // 비어 있으면 키가 암호화되지 않은 것으로 본다
	Fingerprint               string
	KnownHostsFile            string
	InsecureSkipHostKeyVerify bool
	// Log 는 nil 이면 slog.Default(). InsecureSkipHostKeyVerify 경고에 쓴다.
	Log *slog.Logger
}

// sshExecutor 는 머신 하나에 대한 SSH 접속을 유지한다. [§10.1]
type sshExecutor struct {
	client *ssh.Client
}

// NewSSH 는 접속을 맺고 유지하는 Executor 를 만든다. host key 검증 순서는 R20:
// InsecureSkipHostKeyVerify → Fingerprint → KnownHostsFile → 거부. TOFU 는 없다. [§10.1, DESIGN §4.1]
//
// ssh.Dial 의 Timeout 은 TCP 연결에만 적용되고 그 뒤 키 교환·인증에는 상한이 없다
// (x/crypto/ssh 계약). SPEC §10.1 의 "접속 타임아웃 10s"는 시도 1회 전체를 뜻하므로,
// dial 전에 데드라인 하나를 정해 TCP 연결과 NewClientConn(키 교환+인증) 양쪽에 그대로
// 쓴다(각 단계에 새로 10s씩 주면 최악의 경우 거의 20s가 된다). 아래의 재시도도 같은
// 데드라인을 공유한다.
func NewSSH(cfg SSHConfig) (Executor, error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	hostKeyCB, err := sshHostKeyCallback(cfg, log)
	if err != nil {
		return nil, fmt.Errorf("executor/ssh: %w", err)
	}
	signer, err := loadSigner(cfg.KeyFile, cfg.KeyPassphrase)
	if err != nil {
		return nil, fmt.Errorf("executor/ssh: 키 파일 %s: %w", cfg.KeyFile, err)
	}
	clientCfg := &ssh.ClientConfig{
		User:              cfg.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback:   hostKeyCB,
		HostKeyAlgorithms: hostKeyPreference,
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	deadline := time.Now().Add(sshConnectTimeout)
	client, err := dialSSH(addr, clientCfg, deadline)
	// known_hosts 에 그 호스트의 다른 키 타입만 있으면 검증이 "key mismatch" 로 실패한다. 그때
	// KeyError.Want 가 파일이 실제로 가진 키들을 알려주므로, 그 타입들로 한 번 더 시도한다.
	// (파일 매칭 규칙 — 와일드카드·해시 항목 — 은 라이브러리가 이미 안다. 우리가 다시 파싱하지
	// 않는 이유다.) 재시도도 같은 10s 예산 안에서 한다. [§10.1, R20]
	var keyErr *knownhosts.KeyError
	if err != nil && errors.As(err, &keyErr) && len(keyErr.Want) > 0 {
		retryCfg := *clientCfg
		retryCfg.HostKeyAlgorithms = knownKeyTypes(keyErr.Want)
		if len(retryCfg.HostKeyAlgorithms) > 0 {
			log.Debug("executor/ssh: known_hosts 의 키 타입으로 재시도", "host", cfg.Host, "algos", retryCfg.HostKeyAlgorithms)
			client, err = dialSSH(addr, &retryCfg, deadline)
		}
	}
	if err != nil {
		return nil, err
	}
	return &sshExecutor{client: client}, nil
}

// hostKeyPreference 는 OpenSSH 의 host key 선호 순서다. x/crypto 의 기본값은 ed25519 를 마지막에
// 두어 서버가 ecdsa/rsa 를 고르게 만드는데, 사용자가 기록해 둔 fingerprint·known_hosts 항목은
// 보통 OpenSSH 가 협상한 ed25519 다. 순서를 맞추지 않으면 `ssh user@host` 는 되는 정상 호스트가
// gh-ars 에서만 host key 불일치로 거부된다. [§10.1, R20]
var hostKeyPreference = []string{
	ssh.KeyAlgoED25519,
	ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
	ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSA,
}

// knownKeyTypes 는 known_hosts 가 그 호스트에 대해 가진 키 타입들이다(중복 제거, 파일 순서 유지).
func knownKeyTypes(want []knownhosts.KnownKey) []string {
	var out []string
	seen := map[string]bool{}
	for _, k := range want {
		if k.Key == nil {
			continue
		}
		t := k.Key.Type()
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// dialSSH 는 접속 한 번이다. deadline 은 TCP 연결과 키 교환·인증 전체에 걸린다(§10.1 "시도
// 1회당 접속 타임아웃 10s"). ssh.Dial 의 Timeout 은 TCP 에만 적용되므로 직접 건다.
func dialSSH(addr string, clientCfg *ssh.ClientConfig, deadline time.Time) (*ssh.Client, error) {
	conn, err := net.DialTimeout("tcp", addr, time.Until(deadline))
	if err != nil {
		return nil, fmt.Errorf("executor/ssh: %s 접속: %w", addr, err)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("executor/ssh: %s: %w", addr, err)
	}
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, clientCfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("executor/ssh: %s 접속: %w", addr, err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil { // 인증 이후는 ctx 가 Run/Stream 상한을 건다
		_ = sc.Close()
		return nil, fmt.Errorf("executor/ssh: %s: %w", addr, err)
	}
	return ssh.NewClient(sc, chans, reqs), nil
}

// sshHostKeyCallback 은 R20 의 우선순위를 구현한다: 우회(InsecureSkipHostKeyVerify)가
// 있으면 그것으로 끝나고 경고를 남긴다. 없으면 fingerprint → knownHostsFile → 거부
// 순서로 첫 번째로 있는 재료를 쓴다. [§10.1, §6.2]
//
// config 패키지가 이미 이 셋 중 하나는 있음을 정적으로 보장하지만(R20), 이 함수는
// SSHConfig 값만으로 같은 규칙을 판단해 executor 패키지 단독으로도 계약이 성립하게 한다.
func sshHostKeyCallback(cfg SSHConfig, log *slog.Logger) (ssh.HostKeyCallback, error) {
	if cfg.InsecureSkipHostKeyVerify {
		log.Warn("executor/ssh: host key 검증을 건너뜀 (insecureSkipHostKeyVerify, 로컬 테스트 전용)",
			"host", cfg.Host)
		return ssh.InsecureIgnoreHostKey(), nil
	}
	if cfg.Fingerprint != "" {
		want := cfg.Fingerprint
		return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			got := ssh.FingerprintSHA256(key)
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				return fmt.Errorf("host key fingerprint 불일치: got %s, want %s", got, want)
			}
			return nil
		}, nil
	}
	if cfg.KnownHostsFile != "" {
		cb, err := knownhosts.New(cfg.KnownHostsFile)
		if err != nil {
			return nil, fmt.Errorf("known_hosts %s: %w", cfg.KnownHostsFile, err)
		}
		return cb, nil
	}
	return nil, errors.New("host key 검증 재료가 없음: fingerprint 또는 knownHostsFile 이 필요함")
}

// loadSigner 는 개인키 파일을 읽어 Signer 로 만든다. 암호화 여부는 R19 가 config
// 단계에서 이미 판정했으므로 여기서는 passphrase 유무만 보고 맞는 파서를 고른다.
func loadSigner(keyFile, passphrase string) (ssh.Signer, error) {
	pemBytes, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	if passphrase != "" {
		return ssh.ParsePrivateKeyWithPassphrase(pemBytes, []byte(passphrase))
	}
	return ssh.ParsePrivateKey(pemBytes)
}

// shellJoin 은 argv 를 원격 셸이 그대로 재현할 명령 문자열로 만든다. SSH exec 요청은
// 문자열 하나만 실어 원격 셸에 넘기므로(RFC 4254 §6.5), 각 인자를 홑따옴표로
// 감싸 공백·$·; 같은 셸 메타문자가 아무 의미도 갖지 못하게 막는다. JIT config 는
// stdin 으로만 전달되어 argv 에 secret 이 없으므로 이 문자열을 그대로 로그에 남겨도 된다.
func shellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// execMsg 는 SSH "exec" 채널 요청의 페이로드다(RFC 4254 §6.5).
type execMsg struct{ Command string }

// exitSignalMsg 는 SSH "exit-signal" 채널 요청의 페이로드다(RFC 4254 §6.10).
type exitSignalMsg struct {
	Signal     string
	CoreDumped bool
	Error      string
	Lang       string
}

// errPipeDrainTimeout 은 exit-status 도착 뒤에도 stdout/stderr 복사가 상한 안에
// 끝나지 않았음을 알린다. local 의 exec.ErrWaitDelay 와 대응한다.
var errPipeDrainTimeout = errors.New("executor/ssh: 파이프 드레인 상한 초과 (원격이 채널을 물고 있다)")

// withStderr 는 오류 메시지에 캡처된 stderr 를 덧붙인다(있으면). local 의
// exitError 와 같은 형식이다 — 실패한 명령의 진단 정보는 항상 stderr 에 있다. [DESIGN §4.1]
func withStderr(prefix string, err error, stderr string) error {
	if s := strings.TrimSpace(stderr); s != "" {
		return fmt.Errorf("%s: %w: %s", prefix, err, s)
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

// openExecResult 는 openExec 고루틴의 결과다.
type openExecResult struct {
	ch   ssh.Channel
	reqs <-chan *ssh.Request
	err  error
}

// openExec 는 실행 채널을 열고 명령을 시작한다. `ssh.Session` 대신 raw Channel
// API를 쓰는 이유는 Run/Stream 의 doc 참조. ctx 가 먼저 끝나면 접속 전체를 닫아
// 강제로 푼다: 채널 열기·exec 요청의 응답은 라이브러리 내부에서 기다려 개별
// 요청만 취소할 방법이 없다(DESIGN §4.1). 접속을 끊으면 이 머신의 다른 세션도
// 함께 끊기지만, 응답 없는 피어는 사실상 단절과 같아 machine 층의 재접속
// 백오프(§10.1)에 맡긴다.
func (e *sshExecutor) openExec(ctx context.Context, cmdStr string) (ssh.Channel, <-chan *ssh.Request, error) {
	out := make(chan openExecResult, 1)
	go func() {
		ch, reqs, err := e.client.OpenChannel("session", nil)
		if err != nil {
			out <- openExecResult{err: err}
			return
		}
		ok, err := ch.SendRequest("exec", true, ssh.Marshal(&execMsg{Command: cmdStr}))
		if err == nil && !ok {
			err = errors.New("exec 요청이 거부됨")
		}
		if err != nil {
			go func() { _ = ch.Close() }() // Close 의 packet write 자체가 막힐 수 있어 기다리지 않는다
			out <- openExecResult{err: err}
			return
		}
		out <- openExecResult{ch: ch, reqs: reqs}
	}()
	select {
	case r := <-out:
		return r.ch, r.reqs, r.err
	case <-ctx.Done():
		_ = e.client.Close()
		r := <-out // 고루틴 회수(누수 방지)
		if r.ch != nil {
			go func() { _ = r.ch.Close() }()
		}
		return nil, nil, ctx.Err()
	}
}

// waitExit 은 reqs 에서 종료 통지(exit-status 또는 exit-signal, RFC 4254 §6.10)를
// 읽어 종료 코드를 얻는다. 통지가 오면 곧바로 반환한다 — 그 시점부터 §4.1 의
// 파이프 드레인 상한이 시작되므로 reqs 의 나머지(있다면)는 기다리지 않는다.
func waitExit(reqs <-chan *ssh.Request) (exitCode int, err error) {
	for req := range reqs {
		switch req.Type {
		case "exit-status":
			if len(req.Payload) < 4 {
				return -1, errors.New("잘못된 exit-status")
			}
			return int(binary.BigEndian.Uint32(req.Payload)), nil
		case "exit-signal":
			var sig exitSignalMsg
			if err := ssh.Unmarshal(req.Payload, &sig); err != nil {
				return -1, fmt.Errorf("exit-signal 파싱: %w", err)
			}
			return -1, fmt.Errorf("시그널 %s 로 종료", sig.Signal)
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
	return -1, errors.New("원격이 exit-status 없이 채널을 닫음")
}

// waitTwo 는 a·b 가 모두 닫히기를 상한 안에서 기다린다. Run·Stream 각각
// stdout·stderr 복사 고루틴 하나씩, 정확히 둘을 기다리는 데 쓴다.
func waitTwo(a, b <-chan struct{}, timeout time.Duration) bool {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for a != nil || b != nil {
		select {
		case <-a:
			a = nil
		case <-b:
			b = nil
		case <-t.C:
			return false
		}
	}
	return true
}

// waitAllOrClose 는 dones 가 모두 닫히기를 상한 안에서 기다린다. 못 끝나면 접속
// 전체를 닫아(client, 로컬 소켓만 닫아 원격 응답 없이도 즉시 푼다) 나머지를
// 강제로 회수한 뒤에야 반환한다 — 그래서 이 함수가 반환하면 dones 는 항상 전부
// 닫혀 있다(호출자가 그 뒤 goroutine 을 새로 만들지 않아도 된다). [DESIGN §4.1]
func waitAllOrClose(client *ssh.Client, timeout time.Duration, dones ...<-chan struct{}) {
	combined := make(chan struct{})
	go func() {
		for _, d := range dones {
			<-d
		}
		close(combined)
	}()
	select {
	case <-combined:
	case <-time.After(timeout):
		_ = client.Close()
		<-combined
	}
}

// closeChannelBounded 는 채널 종료 요청을 보내고, stdin 복사(전송 중이면)가
// 끝나기를 상한 안에서 기다리되 호출자(Run)를 기다리게 하지 않는다 — 결과는
// 이미 확보했으니 이건 그냥 뒷정리다. `ch.Close()`의 packet write 도, stdin
// 복사의 `ch.Write()`도 SSH 흐름 제어 윈도우가 막히면 무기한 걸릴 수 있어
// (원격이 이미 죽었는데 그 사실을 우리가 모르는 경우), 배경 goroutine 이
// `waitAllOrClose`로 상한을 두고 감시하다가 못 끝나면 접속 전체를 닫는다. [DESIGN §4.1]
func closeChannelBounded(client *ssh.Client, ch ssh.Channel, stdinDone <-chan struct{}, timeout time.Duration) {
	closeReq := make(chan struct{})
	go func() { _ = ch.Close(); close(closeReq) }()
	go waitAllOrClose(client, timeout, closeReq, stdinDone)
}

// Run 은 명령이 끝날 때까지 기다린다. 종료 코드는 오류가 아니라 값이다(Executor 참조).
//
// `ssh.Session` 대신 raw Channel API(`openExec`/`waitExit`)를 쓴다: exit-status
// 도착 시점을 직접 관측해야 §4.1 의 "정상 종료 후 파이프 드레인 상한"을 걸 수
// 있는데, `ssh.Session.Wait`은 채널이 완전히 닫혀야 반환되어(내부 `wait(reqs)`가
// `reqs`가 닫힐 때까지 도는 루프) 그 시점을 공개 API로 노출하지 않는다. 원격
// grandchild 가 stdout/stderr 를 물고 있으면 채널이 안 닫힐 수 있다 — local 의
// `sudo -n podman info` 와 같은 문제(DESIGN §4.1, docs/DECISIONS.md).
func (e *sshExecutor) Run(ctx context.Context, c Cmd) (Result, error) {
	argv, err := resolveArgv(c)
	if err != nil {
		return Result{}, err
	}
	ch, reqs, err := e.openExec(ctx, shellJoin(argv))
	if err != nil {
		return Result{}, fmt.Errorf("executor/ssh: %s: %w", argv[0], err)
	}

	var stdout, stderr bytes.Buffer
	var stdoutErr, stderrErr error
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})
	go func() { _, stdoutErr = io.Copy(&stdout, ch); close(stdoutDone) }()
	go func() { _, stderrErr = io.Copy(&stderr, ch.Stderr()); close(stderrDone) }()
	// stdinDone 은 stdin 복사(또는 그냥 CloseWrite)의 완료를 알린다. `ch.Write()`는
	// SSH 흐름 제어 윈도우가 막히면(원격이 이미 죽었는데 그 사실을 모르는 경우 등)
	// 무기한 걸릴 수 있어(§4.1) 이 완료도 상한 안에서 추적해야 한다 — 그래서
	// CloseWrite 도 기다리지 않고 이 고루틴 안에서만 처리한다.
	stdinDone := make(chan struct{})
	if c.Stdin != nil {
		go func() { _, _ = io.Copy(ch, c.Stdin); _ = ch.CloseWrite(); close(stdinDone) }()
	} else {
		go func() { _ = ch.CloseWrite(); close(stdinDone) }()
	}

	type exitResult struct {
		code int
		err  error
	}
	exitCh := make(chan exitResult, 1)
	go func() {
		code, err := waitExit(reqs)
		exitCh <- exitResult{code, err}
	}()

	select {
	case er := <-exitCh:
		if !waitTwo(stdoutDone, stderrDone, pipeDrainDelay) {
			// Channel.Close() 는 종료 "요청" 메시지만 보낸다 — 로컬 Read 가 실제로
			// 풀리려면(pending.eof) 원격이 close 확인을 보내야 하는데, 원격이 이미
			// grandchild 때문에 막혀 있는 상황이라 그 확인도 늦을 수 있다. 접속
			// 전체(Client.Close, 로컬 소켓만 닫는 동작)를 닫아야 확실히 풀린다. [DESIGN §4.1]
			_ = e.client.Close()
			<-stdoutDone
			<-stderrDone
			<-stdinDone
			return Result{}, withStderr(fmt.Sprintf("executor/ssh: %s: %v 안에 출력이 다 오지 않았다", strings.Join(argv, " "), pipeDrainDelay),
				errPipeDrainTimeout, stderr.String())
		}
		closeChannelBounded(e.client, ch, stdinDone, pipeDrainDelay) // 결과는 이미 확보했다. Close 자체는 상한만 지키면 된다
		// exitCh 를 기다리는 동안이나 드레인 상한(최대 pipeDrainDelay) 동안 ctx 가
		// 끝났을 수 있다. 그새 명령이 실제로 성공했더라도 오류는 "프로세스를 시작
		// 못한 경우와 ctx 취소뿐"이라는 계약대로 ctx 취소를 값보다 우선 보고한다. [DESIGN §4.1]
		if err := ctx.Err(); err != nil {
			return Result{}, fmt.Errorf("executor/ssh: %s: %w", argv[0], err)
		}
		if er.err != nil {
			return Result{}, withStderr(fmt.Sprintf("executor/ssh: %s", argv[0]), er.err, stderr.String())
		}
		// exit-status 는 0이어도 stdout/stderr 를 읽던 도중 진짜 오류(연결 문제 등)가
		// 있었다면 출력이 잘렸을 수 있다 — 값이 아니라 오류로 알린다. [DESIGN §4.1]
		if copyErr := errors.Join(stdoutErr, stderrErr); copyErr != nil {
			return Result{}, withStderr(fmt.Sprintf("executor/ssh: %s: 출력 읽기 실패", strings.Join(argv, " ")), copyErr, stderr.String())
		}
		return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: er.code}, nil
	case <-ctx.Done():
		_ = e.client.Close() // 응답 없는 접속까지 확실히 푼다(openExec 문서 참조)
		<-exitCh             // 고루틴 회수(누수 방지)
		<-stdoutDone
		<-stderrDone
		<-stdinDone
		return Result{}, fmt.Errorf("executor/ssh: %s: %w", argv[0], ctx.Err())
	}
}

// Stream 은 장기 스트림(events)을 연다. 반환 ReadCloser 가 닫히면(원격 종료·단절)
// 호출자가 백오프로 재시작한다. Run 과 같은 이유로 raw Channel API를 쓴다. [§7.1-8]
func (e *sshExecutor) Stream(ctx context.Context, c Cmd) (io.ReadCloser, error) {
	argv, err := resolveArgv(c)
	if err != nil {
		return nil, err
	}
	ch, reqs, err := e.openExec(ctx, shellJoin(argv))
	if err != nil {
		return nil, fmt.Errorf("executor/ssh: %s: %w", argv[0], err)
	}
	outR, outW := io.Pipe()
	stderr := &capBuffer{}
	// stdinDone 은 Run 과 같은 이유로 추적한다: ch.Write() 가 SSH 흐름 제어
	// 윈도우에 막히면 무기한 걸릴 수 있어(§4.1), Close() 가 이 완료도 상한
	// 안에서 기다려야 한다.
	stdinDone := make(chan struct{})
	if c.Stdin != nil {
		go func() { _, _ = io.Copy(ch, c.Stdin); _ = ch.CloseWrite(); close(stdinDone) }()
	} else {
		go func() { _ = ch.CloseWrite(); close(stdinDone) }()
	}
	var stdoutErr, stderrErr error
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})
	go func() { _, stdoutErr = io.Copy(outW, ch); close(stdoutDone) }()
	go func() { _, stderrErr = io.Copy(stderr, ch.Stderr()); close(stderrDone) }()

	s := &sshStream{client: e.client, pr: outR, ch: ch, done: make(chan struct{}), stdinDone: stdinDone}
	go func() {
		defer close(s.done)
		code, exitErr := waitExit(reqs)
		if !waitTwo(stdoutDone, stderrDone, pipeDrainDelay) {
			// Run 과 같은 이유로 채널이 아니라 접속 전체를 닫는다: Channel.Close() 는
			// 요청만 보내고, 로컬 Read 를 실제로 푸는 것은 원격의 close 확인이다. [DESIGN §4.1]
			_ = e.client.Close()
			// outW 를 먼저 닫는다: 복사 고루틴이 (원격이 아니라) 우리 자신의 읽히지
			// 않는 파이프에 막혀 있을 수 있고(호출자가 읽기를 멈춘 경우), 그건
			// client.Close() 가 아니라 outW 를 닫아야 풀린다(io.Pipe 의 Write 는
			// CloseWithError 로 즉시 풀린다).
			errVal := withStderr(fmt.Sprintf("executor/ssh: %s: %v 안에 출력이 다 오지 않았다", strings.Join(argv, " "), pipeDrainDelay),
				errPipeDrainTimeout, stderr.String())
			_ = outW.CloseWithError(errVal)
			<-stdoutDone
			<-stderrDone
			<-stdinDone
			return
		}
		// ch 를 여기서 직접 닫지 않는다: fire-and-forget 으로 띄우면 close(s.done) 가
		// 그 완료를 기다리지 않고 먼저 일어나 Close() 의 상한 로직(아래)이 무력화된다
		// (s.done 이 이미 닫혀 있으니 시간초과 분기를 아예 타지 않는다). 채널 정리는
		// 호출자가 부르는 Close()(상한이 있다)에 맡긴다 — 계약대로 호출자는 다 읽으면 Close 한다.
		if err := sshStreamExitError(argv, code, exitErr, stderr); err != nil {
			_ = outW.CloseWithError(err)
			return
		}
		// exit-status 는 0이어도 stdout/stderr 를 읽던 도중 진짜 오류가 있었다면
		// 출력이 잘렸을 수 있다 — EOF(성공)이 아니라 오류로 알린다. [DESIGN §4.1]
		if copyErr := errors.Join(stdoutErr, stderrErr); copyErr != nil {
			_ = outW.CloseWithError(withStderr(fmt.Sprintf("executor/ssh: %s: 출력 읽기 실패", strings.Join(argv, " ")), copyErr, stderr.String()))
			return
		}
		_ = outW.Close()
	}()
	// ctx 취소도 Close 와 같은 효과를 낸다: 접속을 강제로 닫고 로컬 파이프도 닫는다
	// (그랜드차일드가 물고 있는 것과 무관하게, 읽는 쪽이 멈춘 경우도 마저 푼다).
	// outW 를 (outR 가 아니라) CloseWithError 로 닫아야 마지막 Read 가 argv·stderr·
	// ctx 오류를 받는다 — io.Pipe 는 reader 쪽 Close 가 아니라 writer 쪽
	// CloseWithError 의 오류만 Read 에 전달한다(reader 가 스스로 닫으면 앞으로의
	// Read 는 그냥 ErrClosedPipe 다). Write 쪽에서 닫아도 막혀 있는 Write 는 그대로
	// 풀린다(공유된 done 채널). [DESIGN §4.1]
	go func() {
		select {
		case <-ctx.Done():
			_ = e.client.Close()
			_ = outW.CloseWithError(withStderr(fmt.Sprintf("executor/ssh: %s", strings.Join(argv, " ")), ctx.Err(), stderr.String()))
		case <-s.done:
		}
	}()
	return s, nil
}

// Close 는 접속을 끊는다. 열려 있는 실행 채널(Run/Stream)은 각자 회수한다.
func (e *sshExecutor) Close() error {
	return e.client.Close()
}

// sshStream 은 장기 스트림의 읽기 끝이다. 채널 수명을 함께 소유한다.
type sshStream struct {
	client    *ssh.Client
	pr        *io.PipeReader
	ch        ssh.Channel
	done      chan struct{} // 채널 회수 완료
	stdinDone <-chan struct{}
}

// Read 는 스트림이 끝나면 io.EOF(정상) 또는 종료 사유(비정상)를 낸다.
func (s *sshStream) Read(p []byte) (int, error) { return s.pr.Read(p) }

// Close 는 로컬 파이프를 닫고(대기 중인 쓰기를 즉시 푼다) 채널에 종료를
// 요청한다. DESIGN §4.1 은 Close 가 "ctx 와 무관하게 … 정리를 기다리는 유일한
// 방법"이길 요구하는데, `Channel.Close()`(packet write)도 stdin 복사의
// `ch.Write()`도 전송 계층·SSH 흐름 제어 윈도우가 막히면 무기한 걸릴 수 있다
// (원격이 이미 죽었는데 그 사실을 모르는 경우 등). 그래서 이 셋(스트림 종료
// s.done, close 요청, stdin 복사)을 상한(pipeDrainDelay) 하나로 함께 기다리고
// (`waitAllOrClose`), 시간 안에 못 끝나면 접속 전체(Client.Close, 로컬 소켓만
// 닫아 원격 응답 없이도 즉시 푼다)로 넘어가 나머지를 마저 회수한다. 정상적인
// 경우(원격이 응답함)에는 접속을 유지해 이 머신의 다른 세션(Run 호출)에 영향이
// 없다. 여러 번 불러도 안전하다.
func (s *sshStream) Close() error {
	_ = s.pr.Close()
	closeReq := make(chan struct{})
	go func() { _ = s.ch.Close(); close(closeReq) }()
	waitAllOrClose(s.client, pipeDrainDelay, s.done, closeReq, s.stdinDone)
	return nil
}

// sshStreamExitError 는 채널 종료 사유를 스트림 독자에게 줄 오류로 바꾼다. local 의
// exitError 와 대응한다. 정상 종료(코드 0, err 없음)면 nil 이고, 그때 Read 는 io.EOF 를 받는다.
func sshStreamExitError(argv []string, code int, err error, stderr *capBuffer) error {
	if err != nil {
		return withStderr(fmt.Sprintf("executor/ssh: %s", strings.Join(argv, " ")), err, stderr.String())
	}
	if code != 0 {
		return &cmdError{argv: argv, exitCode: code, stderr: stderr.String()}
	}
	return nil
}
