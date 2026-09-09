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
	"sort"
	"strconv"
	"strings"
	"sync"
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

// keepalive 상수. SPEC §8.3 상수 표를 따른다.
const (
	// sshKeepaliveInterval 은 연결 수준 생존 확인 주기다. [§8.3 "SSH keepalive 주기"]
	sshKeepaliveInterval = 30 * time.Second
	// sshKeepaliveMaxMiss 는 연속 실패 허용 횟수다. 닿으면 단절로 보고 접속을 닫아 §10.1 의
	// "단절 → unhealthy → 재접속" 경로로 보낸다. 첫 주기는 프로브를 보내는 데 쓰이므로 조용히
	// 끊긴 접속을 걷어내는 최악의 시간은 `주기 × (미스 + 1)` = 120s 다. [§8.3 "SSH keepalive 허용 미스"]
	sshKeepaliveMaxMiss = 3
)

// sshExecutor 는 머신 하나에 대한 SSH 접속을 유지한다. [§10.1]
//
// 접속을 닫는 주체는 둘뿐이다: 명시적 Close 와 keepalive 판정(연결 수준 신호). 명령 하나의
// ctx 만료·드레인 상한은 그 채널만 닫는다 — DESIGN §4.1 이 ctx 취소를 명령 단위의 정상적인
// 결과로 규정하고 local 구현도 프로세스만 죽이므로, 여기서 접속을 파괴하면 같은 계약이 두
// 구현에서 다르게 동작하고 events 스트림·동시 실행 중인 다른 명령까지 함께 끊긴다. [DESIGN §4.1, §10.1]
type sshExecutor struct {
	client *ssh.Client
	log    *slog.Logger
	stop   chan struct{} // keepalive 종료
	once   sync.Once
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
		User:            cfg.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCB,
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	deadline := time.Now().Add(sshConnectTimeout)
	// 시도 계획: fingerprint 는 기록된 키의 계열을 알 수 없으므로 계열을 차례로 본다(불일치는
	// "다른 계열이 협상됐다" 일 수 있다). 그 밖의 검증(known_hosts·우회)은 한 번에 전부 제시하고,
	// known_hosts 가 "key mismatch" 를 내면 파일이 실제로 가진 키 계열로 한 번 더 시도한다 —
	// KeyError.Want 가 그것을 알려주므로 파일 매칭 규칙(와일드카드·해시 항목)을 우리가 다시
	// 구현하지 않아도 된다. 모든 시도는 같은 10s 예산 안에서 한다. [§10.1, R20]
	plan := [][]string{hostKeyPreference}
	if cfg.Fingerprint != "" {
		plan = hostKeyFamilies
	}
	var client *ssh.Client
	for _, algos := range plan {
		clientCfg.HostKeyAlgorithms = algos
		client, err = dialSSH(addr, clientCfg, deadline)
		if err == nil {
			break
		}
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) && len(keyErr.Want) > 0 {
			if types := knownKeyTypes(keyErr.Want); len(types) > 0 {
				log.Debug("executor/ssh: known_hosts 의 키 계열로 재시도", "host", cfg.Host, "algos", types)
				clientCfg.HostKeyAlgorithms = types
				client, err = dialSSH(addr, clientCfg, deadline)
			}
			break
		}
		// fingerprint 계획에서는 **어떤 실패든** 다음 계열을 시도한다. 지문 불일치뿐 아니라
		// "no common algorithm for host key"(서버에 그 계열 키가 아예 없는 경우)도 다음 계열에서
		// 풀리기 때문이다 — 그 오류에서 멈추면 ed25519 host key 가 없는 서버에는 영영 못 붙는다.
		// 시도 횟수는 계열 수(3)와 10s 예산이 함께 묶는다.
		if len(plan) == 1 || !time.Now().Before(deadline) {
			break
		}
		log.Debug("executor/ssh: host key 협상 실패, 다음 키 계열로 재시도", "host", cfg.Host, "err", err)
	}
	if err != nil {
		return nil, err
	}
	e := &sshExecutor{client: client, log: log, stop: make(chan struct{})}
	go e.keepalive()
	return e, nil
}

// keepalive 는 연결 수준 생존 확인이다. half-open(조용히 끊긴) 접속은 events 스트림이
// 아무 말도 하지 않고 명령도 TCP 재전송이 끝날 때까지 매달리므로, 머신이 healthy 로 남아
// 계속 배치를 받고 그 unit 들은 기동 타임아웃까지 매달렸다 실패하기를 반복한다. 주기적으로
// keepalive 요청을 보내 연속 실패가 상한을 넘으면 접속을 닫아 단절로 만든다. [§10.1, §8.3]
func (e *sshExecutor) keepalive() {
	t := time.NewTicker(sshKeepaliveInterval)
	defer t.Stop()
	keepaliveLoop(e.stop, t.C, func() error {
		_, _, err := e.client.SendRequest("keepalive@openssh.com", true, nil)
		return err
	}, sshKeepaliveMaxMiss, func(err error) {
		e.log.Warn("executor/ssh: keepalive 연속 실패, 접속을 닫는다", "misses", sshKeepaliveMaxMiss, "err", err)
		_ = e.client.Close()
	})
}

// errKeepaliveNoReply 는 한 주기 안에 응답이 오지 않은 프로브다.
var errKeepaliveNoReply = errors.New("executor/ssh: keepalive 응답 없음")

// keepaliveLoop 은 keepalive 의 판정 부분이다(테스트가 시계와 전송을 넣는다).
//
// 프로브를 tick 안에서 **동기로** 부르면 안 된다: `SendRequest(wantReply=true)` 는 응답이
// 오거나 접속이 죽을 때까지 막히는데, half-open 접속에서는 그 시점이 TCP 스택이 포기할 때
// (Linux 기본 ≈15분)라 §8.3 이 약속한 `주기 × 미스`(90s) 상한이 무너진다. 그래서 프로브는
// goroutine 으로 띄우고, **다음 tick 까지 응답이 없으면 그 자체를 miss 로 센다**
// (OpenSSH 의 ServerAliveInterval/ServerAliveCountMax 와 같은 의미론). 프로브는 한 번에
// 하나만 띄운다 — x/crypto 의 SendRequest 는 직렬화되므로 겹쳐 띄워도 대기만 늘어난다.
func keepaliveLoop(stop <-chan struct{}, tick <-chan time.Time, send func() error, maxMiss int, onDead func(error)) {
	res := make(chan error, 1) // 버려진 프로브가 남아도 쓰기가 막히지 않게 버퍼 1
	var st keepaliveState
	for {
		select {
		case <-stop:
			return
		case err := <-res:
			if st.result(err, maxMiss) {
				onDead(err)
				return
			}
		case <-tick:
			// tick 직전에 도착한 결과가 있으면 먼저 반영한다: 그 프로브는 응답한 것이므로
			// "응답 없음" 으로 세면 안 된다.
			select {
			case err := <-res:
				if st.result(err, maxMiss) {
					onDead(err)
					return
				}
			default:
			}
			probe, dead := st.tick(maxMiss)
			if dead {
				onDead(errKeepaliveNoReply)
				return
			}
			if probe {
				go func() { res <- send() }()
			}
		}
	}
}

// keepaliveState 는 keepalive 판정의 상태 기계다. 시간도 전송도 모르고 세기만 하므로
// 유닛 테스트가 표로 검증한다(루프는 이것을 구동만 한다). [§10.1, §8.3]
type keepaliveState struct {
	miss     int
	inflight bool
}

// result 는 프로브 결과를 반영한다. 상한에 닿으면 true.
func (s *keepaliveState) result(err error, maxMiss int) bool {
	s.inflight = false
	if err == nil {
		s.miss = 0
		return false
	}
	s.miss++
	return s.miss >= maxMiss
}

// tick 은 주기 도래를 반영한다. probe 면 새 프로브를 띄워야 하고, dead 면 상한 도달이다
// (지난 주기의 프로브가 아직 응답하지 않은 것 자체가 miss 다).
func (s *keepaliveState) tick(maxMiss int) (probe, dead bool) {
	if s.inflight {
		s.miss++
		return false, s.miss >= maxMiss
	}
	s.inflight = true
	return true, false
}

// errFingerprintMismatch 는 제시된 host key 의 지문이 설정과 다르다는 뜻이다. 타입을 바꿔
// 한 번 더 시도할지 판단하는 데 쓴다(아래 NewSSH). [§10.1, R20]
var errFingerprintMismatch = errors.New("host key fingerprint 불일치")

// hostKeyPreference 는 x/crypto 가 지원하는 host key 알고리즘을 OpenSSH 순서(ed25519 → ecdsa
// → rsa)로 다시 정렬한 것이다. 목록을 손으로 새로 쓰지 않는다: 그러면 인증서 알고리즘
// (`@cert-authority` known_hosts)이 빠지거나, 라이브러리가 보안 문제로 제외한 `ssh-rsa`
// (SHA-1 서명)를 되살리게 된다.
//
// 정렬이 필요한 이유: x/crypto 의 기본 순서는 ed25519 를 뒤에 두어 서버가 ecdsa/rsa 를 고르는데,
// 사용자가 기록해 둔 known_hosts 항목·fingerprint 는 보통 OpenSSH 가 협상한 ed25519 다. 순서를
// 맞추지 않으면 `ssh user@host` 는 되는 정상 호스트가 gh-ars 에서만 거부된다. [§10.1, R20]
var hostKeyPreference = sortByFamily(ssh.SupportedAlgorithms().HostKeys)

// hostKeyFamilies 는 같은 순서를 계열별로 묶은 것이다. fingerprint 는 어떤 계열의 키를 기록해
// 둔 것인지 알 수 없으므로, 불일치가 나면 다음 계열로 넘어가며 차례로 시도한다. [§10.1, R20]
var hostKeyFamilies = groupByFamily(hostKeyPreference)

// hostKeyFamily 는 알고리즘 이름의 계열 순위다(작을수록 먼저). 인증서 변형은 같은 계열이다.
func hostKeyFamily(algo string) int {
	switch {
	case strings.Contains(algo, "ed25519"):
		return 0
	case strings.Contains(algo, "ecdsa"):
		return 1
	default: // rsa 및 그 밖
		return 2
	}
}

func sortByFamily(algos []string) []string {
	out := append([]string(nil), algos...)
	sort.SliceStable(out, func(i, j int) bool { return hostKeyFamily(out[i]) < hostKeyFamily(out[j]) })
	return out
}

func groupByFamily(sorted []string) [][]string {
	var out [][]string
	for i, algo := range sorted {
		if i == 0 || hostKeyFamily(algo) != hostKeyFamily(sorted[i-1]) {
			out = append(out, nil)
		}
		out[len(out)-1] = append(out[len(out)-1], algo)
	}
	return out
}

// knownKeyTypes 는 known_hosts 가 그 호스트에 대해 가진 키로 **협상 가능한 서명 알고리즘**
// 목록을 만든다(선호 순서 유지, 중복 제거).
//
// RSA 키의 Type() 은 "ssh-rsa"(SHA-1 서명)인데 x/crypto 는 그것을 지원 목록에서 빼 두었고
// OpenSSH 8.8+ 도 기본값에서 끈다. 그래서 같은 계열의 지원 알고리즘(rsa-sha2-256/512)으로
// 펼친다 — 그러지 않으면 RSA 항목만 있는 known_hosts 에서 "no common algorithm" 으로 협상
// 자체가 실패한다. [§10.1, R20]
func knownKeyTypes(want []knownhosts.KnownKey) []string {
	families := map[int]bool{}
	for _, k := range want {
		if k.Key != nil {
			families[hostKeyFamily(k.Key.Type())] = true
		}
	}
	var out []string
	for _, algo := range hostKeyPreference {
		if families[hostKeyFamily(algo)] {
			out = append(out, algo)
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
				return fmt.Errorf("%w: got %s, want %s", errFingerprintMismatch, got, want)
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
// API를 쓰는 이유는 Run/Stream 의 doc 참조. 채널 열기·exec 요청의 응답은 라이브러리
// 내부에서 기다려 개별 요청만 취소할 방법이 없으므로, ctx 가 먼저 끝나면 기다리지 않고
// 반환하고 열기가 끝나는 대로 **그 채널만** 닫는다. 접속은 끊지 않는다 — 그것은 이 머신의
// events 스트림과 다른 명령까지 함께 끊는다(§10.1). 정말 죽은 접속은 keepalive 가 걷어낸다.
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
		// 접속은 건드리지 않는다: 열기가 끝나면 그 채널만 닫는다(열기 자체가 원격 응답을
		// 기다리므로 여기서 기다리지 않고 배경으로 넘긴다). [DESIGN §4.1]
		go func() {
			if r := <-out; r.ch != nil {
				_ = r.ch.Close()
			}
		}()
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

// waitAll 은 dones 가 모두 닫히기를 상한 안에서 기다린다. 못 끝나면 false 를 돌려주고
// **그대로 반환한다**: 남은 goroutine 은 버린다(접속을 닫아 강제로 회수하지 않는다).
//
// 예전에는 여기서 접속을 닫았는데, 그러면 명령 하나의 상한 초과가 그 머신의 events 스트림과
// 동시 실행 중인 다른 명령까지 끊었다(DESIGN §4.1 의 Executor 계약 위반). 버려진 goroutine 은
// 원격이 응답하거나 접속이 끝날 때 함께 사라지고, 접속 자체가 죽어 있으면 keepalive 가 걷어낸다.
// 버려도 안전한 이유: 그들이 쓰는 버퍼(capBuffer)는 잠금이 있고, 호출자는 이 시점 이후 그
// 출력을 값으로 돌려주지 않는다. [DESIGN §4.1, §10.1]
func waitAll(timeout time.Duration, dones ...<-chan struct{}) bool {
	combined := make(chan struct{})
	go func() {
		for _, d := range dones {
			<-d
		}
		close(combined)
	}()
	select {
	case <-combined:
		return true
	case <-time.After(timeout):
		return false
	}
}

// closeChannelBounded 는 채널 종료 요청을 보내고, stdin 복사(전송 중이면)가
// 끝나기를 상한 안에서 기다리되 호출자(Run)를 기다리게 하지 않는다 — 결과는
// 이미 확보했으니 이건 그냥 뒷정리다. `ch.Close()`의 packet write 도, stdin
// 복사의 `ch.Write()`도 SSH 흐름 제어 윈도우가 막히면 무기한 걸릴 수 있어
// (원격이 이미 죽었는데 그 사실을 우리가 모르는 경우), 배경 goroutine 이
// `waitAll`로 상한을 두고 감시하다가 못 끝나면 **그 goroutine 을 버린다**
// (접속은 유지한다 — 죽은 접속은 keepalive 가 걷어낸다). [DESIGN §4.1, §10.1]
func closeChannelBounded(ch ssh.Channel, stdinDone <-chan struct{}, timeout time.Duration) {
	closeReq := make(chan struct{})
	go func() { _ = ch.Close(); close(closeReq) }()
	go waitAll(timeout, closeReq, stdinDone)
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
			// grandchild 때문에 막혀 있는 상황이라 그 확인도 늦을 수 있다. 그래도 접속은
			// 닫지 않는다: 상한까지 기다린 뒤 남은 goroutine 을 버린다. [DESIGN §4.1, §10.1]
			closeChannelBounded(ch, stdinDone, pipeDrainDelay)
			// 상한 안에 안 풀리면 복사 goroutine 은 버린다. 접속을 닫아 강제 회수하지 않는다:
			// 그것은 이 머신의 events 스트림과 다른 명령까지 끊는다. [DESIGN §4.1]
			recovered := waitAll(pipeDrainDelay, stdoutDone, stderrDone, stdinDone)
			// 버린 goroutine 이 아직 쓰고 있을 수 있으므로 그때는 버퍼를 읽지 않는다(bytes.Buffer 는
			// 잠금이 없다). 회수됐을 때만 stderr 를 오류에 싣는다.
			detail := ""
			if recovered {
				detail = stderr.String()
			}
			return Result{}, withStderr(fmt.Sprintf("executor/ssh: %s: %v 안에 출력이 다 오지 않았다", strings.Join(argv, " "), pipeDrainDelay),
				errPipeDrainTimeout, detail)
		}
		closeChannelBounded(ch, stdinDone, pipeDrainDelay) // 결과는 이미 확보했다. Close 자체는 상한만 지키면 된다
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
		// 명령 하나의 ctx 만료는 그 채널만 닫는다. 접속을 닫으면 §8.3 이 "Dying 유지 + 다음 tick
		// 재시도" 로 규정한 국소적 실패가 머신 unhealthy·capacity 감소·전체 재동기화로 번진다.
		// 상한 안에 회수되지 않은 goroutine 은 버린다(위 waitAll 주석). [DESIGN §4.1, §8.3]
		closeChannelBounded(ch, stdinDone, pipeDrainDelay)
		exitDone := make(chan struct{})
		go func() { <-exitCh; close(exitDone) }()
		waitAll(pipeDrainDelay, exitDone, stdoutDone, stderrDone, stdinDone)
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

	s := &sshStream{pr: outR, ch: ch, done: make(chan struct{}), stdinDone: stdinDone}
	go func() {
		defer close(s.done)
		code, exitErr := waitExit(reqs)
		if !waitTwo(stdoutDone, stderrDone, pipeDrainDelay) {
			// 채널만 닫는다. Channel.Close() 는 요청만 보내므로 원격이 응답하지 않으면 복사
			// goroutine 이 남지만, 그것은 버린다(접속을 닫으면 이 머신 전체가 끊긴다). 죽은
			// 접속은 keepalive 가 걷어낸다. [DESIGN §4.1, §10.1]
			go func() { _ = ch.Close() }()
			// outW 를 먼저 닫는다: 복사 고루틴이 (원격이 아니라) 우리 자신의 읽히지
			// 않는 파이프에 막혀 있을 수 있고(호출자가 읽기를 멈춘 경우), 그건
			// client.Close() 가 아니라 outW 를 닫아야 풀린다(io.Pipe 의 Write 는
			// CloseWithError 로 즉시 풀린다).
			errVal := withStderr(fmt.Sprintf("executor/ssh: %s: %v 안에 출력이 다 오지 않았다", strings.Join(argv, " "), pipeDrainDelay),
				errPipeDrainTimeout, stderr.String())
			_ = outW.CloseWithError(errVal)
			waitAll(pipeDrainDelay, stdoutDone, stderrDone, stdinDone)
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
	// ctx 취소도 Close 와 같은 효과를 낸다: 이 스트림의 채널과 로컬 파이프를 닫는다(접속은 유지)
	// (그랜드차일드가 물고 있는 것과 무관하게, 읽는 쪽이 멈춘 경우도 마저 푼다).
	// outW 를 (outR 가 아니라) CloseWithError 로 닫아야 마지막 Read 가 argv·stderr·
	// ctx 오류를 받는다 — io.Pipe 는 reader 쪽 Close 가 아니라 writer 쪽
	// CloseWithError 의 오류만 Read 에 전달한다(reader 가 스스로 닫으면 앞으로의
	// Read 는 그냥 ErrClosedPipe 다). Write 쪽에서 닫아도 막혀 있는 Write 는 그대로
	// 풀린다(공유된 done 채널). [DESIGN §4.1]
	go func() {
		select {
		case <-ctx.Done():
			// 스트림의 채널만 닫는다(접속 유지). 읽는 쪽은 outW 를 닫아 즉시 푼다. [DESIGN §4.1]
			go func() { _ = ch.Close() }()
			_ = outW.CloseWithError(withStderr(fmt.Sprintf("executor/ssh: %s", strings.Join(argv, " ")), ctx.Err(), stderr.String()))
		case <-s.done:
		}
	}()
	return s, nil
}

// Close 는 접속을 끊는다. 열려 있는 실행 채널(Run/Stream)은 각자 회수한다. 여러 번 불러도 안전하다.
func (e *sshExecutor) Close() error {
	e.once.Do(func() { close(e.stop) })
	return e.client.Close()
}

// sshStream 은 장기 스트림의 읽기 끝이다. 채널 수명을 함께 소유한다.
type sshStream struct {
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
// (`waitAll`), 시간 안에 못 끝나면 남은 goroutine 을 버리고 반환한다 — 접속은
// 어느 경우에도 닫지 않는다(그것은 이 머신의 다른 세션까지 끊는다). 죽은 접속은
// keepalive 가 걷어낸다. 여러 번 불러도 안전하다. [DESIGN §4.1, §10.1]
func (s *sshStream) Close() error {
	_ = s.pr.Close()
	closeReq := make(chan struct{})
	go func() { _ = s.ch.Close(); close(closeReq) }()
	waitAll(pipeDrainDelay, s.done, closeReq, s.stdinDone)
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
