package executor

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// genKey 는 테스트용 host key 하나를 만든다.
func genKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh.NewPublicKey: %v", err)
	}
	return sshPub
}

func dummyAddr(t *testing.T) net.Addr {
	t.Helper()
	addr, err := net.ResolveTCPAddr("tcp", "203.0.113.5:22")
	if err != nil {
		t.Fatalf("ResolveTCPAddr: %v", err)
	}
	return addr
}

// [R20, §10.1] fingerprint 가 있으면 knownHostsFile 이 있어도 fingerprint 로만 판정한다.
func TestSSHHostKeyCallback_R20_FingerprintFirst(t *testing.T) {
	key := genKey(t)
	other := genKey(t)
	dir := t.TempDir()
	knownHosts := filepath.Join(dir, "known_hosts")
	// known_hosts 에는 다른 키를 등록해 둔다: fingerprint 가 우선이라면 이 파일은 안 쓰인다.
	if err := os.WriteFile(knownHosts, []byte(knownhosts.Line([]string{"host:22"}, other)+"\n"), 0o600); err != nil {
		t.Fatalf("known_hosts 쓰기: %v", err)
	}
	cfg := SSHConfig{Host: "host", Fingerprint: ssh.FingerprintSHA256(key), KnownHostsFile: knownHosts}
	cb, err := sshHostKeyCallback(cfg, slog.Default())
	if err != nil {
		t.Fatalf("sshHostKeyCallback: %v", err)
	}
	if err := cb("host:22", dummyAddr(t), key); err != nil {
		t.Fatalf("일치하는 fingerprint 인데 거부됨: %v", err)
	}
	if err := cb("host:22", dummyAddr(t), other); err == nil {
		t.Fatal("fingerprint 불일치인데 허용됨")
	}
}

// [R20, §10.1] fingerprint 가 없으면 knownHostsFile 로 판정한다.
func TestSSHHostKeyCallback_R20_KnownHostsSecond(t *testing.T) {
	key := genKey(t)
	other := genKey(t)
	dir := t.TempDir()
	knownHosts := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte(knownhosts.Line([]string{"host:22"}, key)+"\n"), 0o600); err != nil {
		t.Fatalf("known_hosts 쓰기: %v", err)
	}
	cfg := SSHConfig{Host: "host", KnownHostsFile: knownHosts}
	cb, err := sshHostKeyCallback(cfg, slog.Default())
	if err != nil {
		t.Fatalf("sshHostKeyCallback: %v", err)
	}
	if err := cb("host:22", dummyAddr(t), key); err != nil {
		t.Fatalf("known_hosts 에 등록된 키인데 거부됨: %v", err)
	}
	var keyErr *knownhosts.KeyError
	if err := cb("host:22", dummyAddr(t), other); !errors.As(err, &keyErr) {
		t.Fatalf("등록되지 않은 키인데 허용됨: err=%v", err)
	}
}

// [R20] 둘 다 없으면 접속을 거부한다(TOFU 없음).
func TestSSHHostKeyCallback_R20_RejectWhenNeither(t *testing.T) {
	cfg := SSHConfig{Host: "host"}
	if _, err := sshHostKeyCallback(cfg, slog.Default()); err == nil {
		t.Fatal("fingerprint·knownHostsFile 둘 다 없는데 허용됨")
	}
}

// [R20] InsecureSkipHostKeyVerify 는 유일한 우회이고 경고를 남긴다.
func TestSSHHostKeyCallback_R20_InsecureSkipWarns(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	cfg := SSHConfig{Host: "host", InsecureSkipHostKeyVerify: true}
	cb, err := sshHostKeyCallback(cfg, log)
	if err != nil {
		t.Fatalf("sshHostKeyCallback: %v", err)
	}
	if !strings.Contains(buf.String(), "insecureSkipHostKeyVerify") {
		t.Fatalf("경고 로그가 없다: %q", buf.String())
	}
	// 우회이므로 아무 키나 받는다.
	if err := cb("host:22", dummyAddr(t), genKey(t)); err != nil {
		t.Fatalf("우회했는데 거부됨: %v", err)
	}
}

// [R20] InsecureSkipHostKeyVerify 가 fingerprint·knownHostsFile 보다 앞선다
// (config 의 경고 판정과 같은 우선순위: 값이 있어도 우회 플래그가 이긴다).
func TestSSHHostKeyCallback_R20_InsecureOverridesOthers(t *testing.T) {
	key := genKey(t)
	cfg := SSHConfig{Host: "host", InsecureSkipHostKeyVerify: true, Fingerprint: ssh.FingerprintSHA256(genKey(t))}
	cb, err := sshHostKeyCallback(cfg, slog.Default())
	if err != nil {
		t.Fatalf("sshHostKeyCallback: %v", err)
	}
	// fingerprint 는 다른 키 것인데도, 우회이므로 검증 자체를 안 한다.
	if err := cb("host:22", dummyAddr(t), key); err != nil {
		t.Fatalf("우회했는데 거부됨: %v", err)
	}
}
