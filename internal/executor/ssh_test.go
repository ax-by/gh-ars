package executor

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
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

// genRSAKey 는 테스트용 RSA host key 다(2048비트: 생성 비용과 현실성의 절충).
func genRSAKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	pub, err := ssh.NewPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("ssh.NewPublicKey: %v", err)
	}
	return pub
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

// TestSSH_R20_FingerprintMismatchIsRetryable: fingerprint 불일치는 재시도 판단이 가능한
// 오류여야 한다. 선호 순서를 ed25519 우선으로 고정했으므로, ecdsa/rsa 키의 지문을 기록해 둔
// 머신은 첫 협상에서 반드시 불일치가 난다 — 그때 나머지 타입으로 한 번 더 시도하지 않으면
// 그 머신은 영구히 접속하지 못한다(이 선호 순서 도입 전에는 되던 구성이다). [§10.1, R20]
func TestSSH_R20_FingerprintMismatchIsRetryable(t *testing.T) {
	cb, err := sshHostKeyCallback(SSHConfig{Fingerprint: "SHA256:다른키지문"}, slog.Default())
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	err = cb("host", dummyAddr(t), genKey(t))
	if err == nil {
		t.Fatal("지문이 다른데 통과했다")
	}
	if !errors.Is(err, errFingerprintMismatch) {
		t.Fatalf("err = %v, want errFingerprintMismatch (재시도 판단 불가)", err)
	}
	// 재시도는 계열 단위로 넘어간다: 첫 계열(ed25519)이 안 맞으면 ecdsa, 그 다음 rsa 를 본다.
	// 계열이 하나뿐이면 지문이 3순위 키로 기록된 머신이 영구히 접속하지 못한다.
	if len(hostKeyFamilies) < 3 {
		t.Fatalf("계열 묶음 %d개, want 3 (ed25519·ecdsa·rsa)", len(hostKeyFamilies))
	}
	for i, fam := range hostKeyFamilies {
		if len(fam) == 0 || hostKeyFamily(fam[0]) != i {
			t.Fatalf("계열 %d 이 순서대로가 아니다: %v", i, fam)
		}
	}
}

// TestSSH_R20_KnownKeyTypes: known_hosts 가 가진 키의 **계열**로 재시도 목록을 만든다.
// 목록은 라이브러리 지원 목록에서 고른다: 손으로 쓰면 인증서 알고리즘이 빠지거나(`@cert-authority`
// 호스트 접속 불가) 보안 문제로 제외된 `ssh-rsa`(SHA-1)를 되살린다. [§10.1, R20]
func TestSSH_R20_KnownKeyTypes(t *testing.T) {
	supported := map[string]bool{}
	for _, a := range ssh.SupportedAlgorithms().HostKeys {
		supported[a] = true
	}

	for _, tc := range []struct {
		name   string
		key    ssh.PublicKey
		family int
	}{
		{"ed25519", genKey(t), 0},
		{"rsa", genRSAKey(t), 2},
	} {
		got := knownKeyTypes([]knownhosts.KnownKey{{Key: tc.key}, {Key: tc.key}, {Key: nil}})
		if len(got) == 0 {
			t.Fatalf("%s: 재시도 목록이 비었다", tc.name)
		}
		seen := map[string]bool{}
		for _, algo := range got {
			if !supported[algo] {
				t.Fatalf("%s: 지원하지 않는 알고리즘 %q (ssh-rsa 같은 것을 되살리면 안 된다)", tc.name, algo)
			}
			if hostKeyFamily(algo) != tc.family {
				t.Fatalf("%s: 다른 계열 %q 가 섞였다", tc.name, algo)
			}
			if seen[algo] {
				t.Fatalf("%s: 중복 %q", tc.name, algo)
			}
			seen[algo] = true
		}
		if tc.family == 2 && seen[ssh.KeyAlgoRSA] {
			t.Fatal("SHA-1 서명(ssh-rsa)을 제시했다")
		}
	}
	if len(knownKeyTypes(nil)) != 0 {
		t.Fatal("항목이 없으면 빈 목록이어야 한다(그래야 기본 선호 순서를 그대로 쓴다)")
	}
}

// TestSSH_R20_HostKeyPreferenceCoversLibrary: 선호 목록은 라이브러리 지원 목록의 재정렬이어야
// 한다(빠지거나 더해지면 인증서 호스트 접속 불가·SHA-1 부활로 이어진다). ed25519 가 먼저다.
func TestSSH_R20_HostKeyPreferenceCoversLibrary(t *testing.T) {
	lib := ssh.SupportedAlgorithms().HostKeys
	if len(hostKeyPreference) != len(lib) {
		t.Fatalf("선호 목록 %d개, 라이브러리 %d개 — 재정렬이 아니라 다른 목록이다", len(hostKeyPreference), len(lib))
	}
	inLib := map[string]bool{}
	for _, a := range lib {
		inLib[a] = true
	}
	for _, a := range hostKeyPreference {
		if !inLib[a] {
			t.Fatalf("라이브러리에 없는 알고리즘 %q", a)
		}
	}
	if hostKeyFamily(hostKeyPreference[0]) != 0 {
		t.Fatalf("첫 알고리즘 %q 가 ed25519 계열이 아니다", hostKeyPreference[0])
	}
	if len(hostKeyFamilies) < 2 {
		t.Fatalf("계열 묶음 %d개 — fingerprint 재시도가 성립하지 않는다", len(hostKeyFamilies))
	}
}
