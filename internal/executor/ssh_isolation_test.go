package executor

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// testSSHServer 는 in-process SSH 서버다. exec 명령 문자열로 동작을 나눈다:
// "stream" 은 닫힐 때까지 주기적으로 한 줄씩 쓰고, 그 밖의 명령은 아무 응답도 하지 않는다
// (원격이 매달린 상황 = 호출자의 ctx 가 만료되는 경우).
type testSSHServer struct {
	ln      net.Listener
	hostKey ssh.Signer
}

// ecdsaHostKey 는 ed25519 가 아닌 host key 다(계열 재시도 검증용).
func ecdsaHostKey(t *testing.T) ssh.Signer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatalf("NewSignerFromKey: %v", err)
	}
	return signer
}

func startTestSSHServer(t *testing.T) *testSSHServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("호스트 키: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("NewSignerFromKey: %v", err)
	}
	return startTestSSHServerWithKey(t, signer)
}

func startTestSSHServerWithKey(t *testing.T, signer ssh.Signer) *testSSHServer {
	t.Helper()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil },
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &testSSHServer{ln: ln, hostKey: signer}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serveConn(conn, cfg)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *testSSHServer) serveConn(conn net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		_ = conn.Close()
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs) // keepalive 요청에 실패 응답을 돌려준다(= 연결은 살아 있음)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			return
		}
		go s.serveSession(ch, chReqs)
	}
}

func (s *testSSHServer) serveSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	for req := range reqs {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		_ = req.Reply(true, nil)
		var payload struct{ Command string }
		_ = ssh.Unmarshal(req.Payload, &payload)
		if strings.Contains(payload.Command, "stream") {
			go func() {
				defer ch.Close()
				for {
					if _, err := ch.Write([]byte("tick\n")); err != nil {
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
			}()
			continue
		}
		// 그 밖의 명령: 응답도 종료 통지도 보내지 않는다(원격이 매달린 상태).
	}
}

func (s *testSSHServer) port(t *testing.T) int {
	t.Helper()
	_, p, err := net.SplitHostPort(s.ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	return n
}

// writeTestKey 는 클라이언트 개인키 파일을 만든다(암호 없음).
func writeTestKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("키 생성: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("키 파일: %v", err)
	}
	return path
}

// TestSSH_S4_1_CommandTimeoutKeepsConnection: 명령 하나의 ctx 만료가 접속을 끊지 않는다.
// DESIGN §4.1 은 ctx 취소를 명령 단위의 정상적인 결과로 규정하고(local 은 프로세스만 죽인다),
// SPEC §8.3 은 정리 실패를 "Dying 유지 + 다음 tick 재시도" 로 규정한다. 접속을 닫으면 그
// 국소적 실패가 events 스트림 종료 → 머신 unhealthy → capacity 감소 → 전체 재동기화로 번진다.
func TestSSH_S4_1_CommandTimeoutKeepsConnection(t *testing.T) {
	srv := startTestSSHServer(t)
	ex, err := NewSSH(SSHConfig{
		Host: "127.0.0.1", Port: srv.port(t), User: "tester",
		KeyFile: writeTestKey(t), InsecureSkipHostKeyVerify: true,
	})
	if err != nil {
		t.Fatalf("NewSSH: %v", err)
	}
	defer ex.Close()

	rc, err := ex.Stream(context.Background(), Cmd{Argv: []string{"docker", "events", "stream"}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer rc.Close()
	buf := make([]byte, 5)
	if _, err := rc.Read(buf); err != nil { // 스트림이 살아 있음을 먼저 확인
		t.Fatalf("스트림 첫 읽기: %v", err)
	}

	// 응답 없는 명령: ctx 만료로 실패해야 한다.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := ex.Run(ctx, Cmd{Argv: []string{"docker", "rm", "-f", "hang"}}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run err = %v, want DeadlineExceeded", err)
	}

	// 핵심: 그 뒤에도 같은 접속의 events 스트림이 살아 있어야 한다.
	done := make(chan error, 1)
	go func() {
		_, rerr := rc.Read(buf)
		done <- rerr
	}()
	select {
	case rerr := <-done:
		if rerr != nil {
			t.Fatalf("명령 타임아웃이 스트림을 끊었다: %v", rerr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("스트림에서 더 이상 데이터가 오지 않는다(접속이 끊겼을 가능성)")
	}

	// 새 명령도 여전히 실행할 수 있어야 한다(접속 재사용).
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	if _, err := ex.Run(ctx2, Cmd{Argv: []string{"docker", "ps"}}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("두 번째 Run err = %v, want DeadlineExceeded (접속은 살아 있어야 한다)", err)
	}
}

// TestSSH_R20_FingerprintTriesAllFamilies: 서버에 ed25519 host key 가 없어도(첫 계열 협상
// 실패) 다음 계열로 넘어가 접속한다. 첫 실패에서 멈추면 ecdsa/rsa 만 가진 서버에 영영 못 붙는다.
// [§10.1, R20]
func TestSSH_R20_FingerprintTriesAllFamilies(t *testing.T) {
	srv := startTestSSHServerWithKey(t, ecdsaHostKey(t)) // ed25519 없음
	fp := ssh.FingerprintSHA256(srv.hostKey.PublicKey())
	ex, err := NewSSH(SSHConfig{
		Host: "127.0.0.1", Port: srv.port(t), User: "tester",
		KeyFile: writeTestKey(t), Fingerprint: fp,
	})
	if err != nil {
		t.Fatalf("ed25519 없는 서버에 접속하지 못했다: %v", err)
	}
	_ = ex.Close()
}

// TestSSH_S10_1_KeepaliveState: 판정 규칙 — 실패는 miss 를 올리고, 성공은 되돌리고, 응답하지
// 않은 프로브는 주기마다 miss 로 센다. 상한에 닿으면 단절이다. [§10.1, §8.3]
func TestSSH_S10_1_KeepaliveState(t *testing.T) {
	fail := errors.New("broken pipe")
	const maxMiss = 3

	var st keepaliveState
	if probe, dead := st.tick(maxMiss); !probe || dead {
		t.Fatalf("첫 tick: probe=%v dead=%v, want true/false", probe, dead)
	}
	if st.result(fail, maxMiss) {
		t.Fatal("miss 1회로 단절 판정했다")
	}
	if probe, _ := st.tick(maxMiss); !probe {
		t.Fatal("응답을 받은 뒤에는 새 프로브를 띄워야 한다")
	}
	if st.result(nil, maxMiss) || st.miss != 0 {
		t.Fatalf("성공인데 miss=%d, want 0 (카운터 리셋)", st.miss)
	}

	// 응답하지 않는 프로브: tick 마다 miss 가 늘고 새 프로브는 띄우지 않는다.
	if probe, _ := st.tick(maxMiss); !probe {
		t.Fatal("프로브를 띄워야 한다")
	}
	for i := 1; i <= maxMiss; i++ { // 응답 없는 주기가 maxMiss 번 쌓이면 단절
		probe, dead := st.tick(maxMiss)
		if probe {
			t.Fatalf("[%d] 응답 없는 프로브가 있는데 또 띄웠다", i)
		}
		if dead != (i == maxMiss) {
			t.Fatalf("[%d] dead=%v (miss=%d), want %v", i, dead, st.miss, i == maxMiss)
		}
	}
}

// TestSSH_S10_1_KeepaliveNoReplyCountsAsMiss: 응답이 오지 않는 프로브도 주기마다 miss 로 센다.
// SendRequest 는 half-open 접속에서 TCP 가 포기할 때(수 분~15분)까지 막히므로, 응답을 동기로
// 기다리면 §8.3 이 약속한 `주기 × 미스` 상한이 무너진다. [§10.1, §8.3]
func TestSSH_S10_1_KeepaliveNoReplyCountsAsMiss(t *testing.T) {
	tick := make(chan time.Time)
	stop := make(chan struct{})
	defer close(stop)
	dead := make(chan error, 1)
	block := make(chan struct{}) // send 는 영원히 반환하지 않는다
	defer close(block)

	go keepaliveLoop(stop, tick, func() error {
		<-block
		return nil
	}, 3, func(err error) { dead <- err })

	for i := 0; i < 3; i++ { // 1회차: 프로브 발사, 2·3회차: 응답 없음 → miss 2회
		tick <- time.Now()
	}
	select {
	case <-dead:
		t.Fatal("아직 miss 2회인데 단절로 판정했다")
	case <-time.After(100 * time.Millisecond):
	}
	tick <- time.Now() // miss 3회
	select {
	case err := <-dead:
		if !errors.Is(err, errKeepaliveNoReply) {
			t.Fatalf("onDead err = %v, want 응답 없음", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("응답 없는 프로브를 miss 로 세지 않는다(무한 대기)")
	}
}

// TestSSH_S10_1_KeepaliveStops: stop 이 닫히면 루프가 끝난다(Close 경로).
func TestSSH_S10_1_KeepaliveStops(t *testing.T) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		keepaliveLoop(stop, make(chan time.Time), func() error { return nil }, 3, func(error) {
			t.Error("stop 뒤에 단절 판정이 나왔다")
		})
		close(done)
	}()
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop 에 반응하지 않는다")
	}
}
