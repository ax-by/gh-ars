package config

import (
	"encoding/binary"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gh-ars/internal/domain"
)

// ---------------------------------------------------------------------------
// 테스트 보조: 주입 가능한 Env, 기본 YAML, 키 파일 fixture
// ---------------------------------------------------------------------------

const testHome = "/home/test"

// fakeEnv 는 OS 대신 쓰는 env·파일·홈 디렉터리다. 유닛 테스트는 OS 에 의존하지 않는다.
type fakeEnv struct {
	env   map[string]string
	files map[string][]byte
}

func (f fakeEnv) Env() Env {
	return Env{
		LookupEnv: func(name string) (string, bool) {
			v, ok := f.env[name]
			return v, ok
		},
		ReadFile: func(p string) ([]byte, error) {
			b, ok := f.files[p]
			if !ok {
				return nil, os.ErrNotExist
			}
			return b, nil
		},
		HomeDir: func() (string, error) { return testHome, nil },
	}
}

func homePath(rel string) string { return filepath.Join(testHome, rel) }

func defaultEnv() fakeEnv {
	return fakeEnv{
		env:   map[string]string{"GITHUB_PAT": "ghp_test"},
		files: map[string][]byte{homePath(".ssh/ci"): opensshKey("none", "none")},
	}
}

// minimalYAML 은 local 머신 1대짜리 최소 유효 설정이다.
const minimalYAML = `
github:
  url: https://github.com/my-org
  auth:
    token: "${env:GITHUB_PAT}"
scaleSets:
  - name: linux-x64
    maxRunners: 4
    resources: { cpu: 2, memory: 4Gi }
machines:
  - name: box1
    scaleSet: linux-x64
`

// sshYAML 은 SSH 머신 1대짜리 최소 유효 설정이다.
const sshYAML = `
github:
  url: https://github.com/my-org
  auth:
    token: "${env:GITHUB_PAT}"
scaleSets:
  - name: linux-x64
    maxRunners: 4
    resources: { cpu: 2, memory: 4Gi }
machineDefaults:
  ssh:
    user: runner
    keyFile: ~/.ssh/ci
machines:
  - name: box1
    scaleSet: linux-x64
    host: 10.0.0.12
`

func parse(t *testing.T, yaml string, env fakeEnv) (*Config, error) {
	t.Helper()
	return Parse([]byte(yaml), env.Env())
}

func mustParse(t *testing.T, yaml string, env fakeEnv) *Config {
	t.Helper()
	cfg, err := parse(t, yaml, env)
	if err != nil {
		t.Fatalf("Parse 실패: %v", err)
	}
	return cfg
}

func wantRule(t *testing.T, err error, rule string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s 오류를 기대했지만 nil", rule)
	}
	for _, r := range Rules(err) {
		if r == rule {
			return
		}
	}
	t.Fatalf("%s 오류를 기대했지만 실제: %v", rule, err)
}

func wantNoRule(t *testing.T, err error, rule string) {
	t.Helper()
	for _, r := range Rules(err) {
		if r == rule {
			t.Fatalf("%s 오류가 없어야 하지만 실제: %v", rule, err)
		}
	}
}

// opensshKey 는 openssh-key-v1 형식의 머리 부분(magic, ciphername, kdfname)만 갖춘 합성 PEM 이다.
// R19 판정은 머리 부분만 읽으므로 나머지는 비워 둔다.
func opensshKey(cipher, kdf string) []byte {
	var b []byte
	b = append(b, "openssh-key-v1\x00"...)
	for _, s := range []string{cipher, kdf} {
		b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
		b = append(b, s...)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: b})
}

func pemKey(typ string, headers map[string]string) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Headers: headers, Bytes: []byte("xx")})
}

// ---------------------------------------------------------------------------
// §6.1 예시 파일 로드 (Phase 2 완료 기준)
// ---------------------------------------------------------------------------

// [§6.1] examples/gh-ars.yaml 이 로드되고 기본값·치환·해석 결과가 문서와 일치한다.
func TestParse_Example_S6_1(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "gh-ars.yaml"))
	if err != nil {
		t.Fatalf("예시 파일 읽기 실패: %v", err)
	}
	cfg, err := Parse(data, defaultEnv().Env())
	if err != nil {
		t.Fatalf("예시 파일 Parse 실패: %v", err)
	}

	gh := cfg.GitHub
	if gh.Scope != ScopeOrg || gh.Owner != "my-org" || gh.Repo != "" {
		t.Fatalf("scope = %s/%s/%s, want org/my-org/''", gh.Scope, gh.Owner, gh.Repo)
	}
	if gh.Auth.Token != "ghp_test" || gh.Auth.App != nil {
		t.Fatalf("auth 치환 결과가 다름: token=%q app=%v", gh.Auth.Token, gh.Auth.App)
	}

	if len(cfg.ScaleSets) != 2 {
		t.Fatalf("scaleSets = %d, want 2", len(cfg.ScaleSets))
	}
	ss := cfg.ScaleSets[0]
	if ss.Name != "linux-x64" || ss.RunnerGroup != "Default" || ss.MinRunners != 0 || ss.MaxRunners != 10 {
		t.Fatalf("scaleSets[0] = %+v", ss)
	}
	if ss.Unit.CPU != 2 || ss.Unit.MemoryBytes != 4<<30 {
		t.Fatalf("scaleSets[0].Unit = %+v", ss.Unit)
	}
	if ss.RunnerImage != DefaultRunnerImage || ss.Mode != domain.ModeNone || ss.SidecarImage != "" {
		t.Fatalf("scaleSets[0] image/mode = %q %q %q", ss.RunnerImage, ss.Mode, ss.SidecarImage)
	}
	if got, want := ss.Machines, []string{"build-1", "build-2"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("scaleSets[0].Machines = %v, want %v", got, want)
	}
	dind := cfg.ScaleSets[1]
	if dind.Mode != domain.ModeSidecar || dind.SidecarImage != DefaultSidecarImage[domain.RuntimeDocker] {
		t.Fatalf("scaleSets[1] mode=%q sidecar=%q", dind.Mode, dind.SidecarImage)
	}
	if dind.Unit.CPU != 4 || dind.Unit.MemoryBytes != 8<<30 || dind.MaxRunners != 4 {
		t.Fatalf("scaleSets[1] = %+v", dind)
	}

	if len(cfg.Machines) != 3 {
		t.Fatalf("machines = %d, want 3", len(cfg.Machines))
	}
	m1 := cfg.Machines[0]
	if m1.Local || m1.SSH == nil || m1.Runtime != domain.RuntimeDocker || m1.Resources != nil || m1.MaxRunners != nil {
		t.Fatalf("machines[0] = %+v", m1)
	}
	wantSSH := SSH{Host: "172.18.0.100", Port: 22, User: "runner", KeyFile: homePath(".ssh/ci"), KnownHostsFile: homePath(".ssh/known_hosts")}
	if *m1.SSH != wantSSH {
		t.Fatalf("machines[0].SSH = %+v, want %+v", *m1.SSH, wantSSH)
	}
	m2 := cfg.Machines[1]
	if m2.Runtime != domain.RuntimePodman || m2.MaxRunners == nil || *m2.MaxRunners != 2 {
		t.Fatalf("machines[1] = %+v", m2)
	}
	if m2.Resources == nil || m2.Resources.CPU != 4 || m2.Resources.MemoryBytes != 8<<30 {
		t.Fatalf("machines[1].Resources = %+v", m2.Resources)
	}
	if !strings.HasPrefix(m2.SSH.Fingerprint, "SHA256:") || m2.SSH.Host != "10.0.0.12" || m2.SSH.User != "runner" {
		t.Fatalf("machines[1].SSH = %+v", *m2.SSH)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("경고 없음을 기대: %v", cfg.Warnings)
	}
}

// [§6.0] 기본값 표: runnerGroup, minRunners, runner.image, jobRuntime.mode, ssh.port, knownHostsFile, runtime.
func TestParse_Defaults_S6_0(t *testing.T) {
	cfg := mustParse(t, sshYAML, defaultEnv())
	ss := cfg.ScaleSets[0]
	if ss.RunnerGroup != "Default" || ss.MinRunners != 0 || ss.RunnerImage != DefaultRunnerImage || ss.Mode != domain.ModeNone {
		t.Fatalf("scaleSet 기본값 = %+v", ss)
	}
	m := cfg.Machines[0]
	if m.Runtime != domain.RuntimeDocker {
		t.Fatalf("runtime 기본값 = %q, want docker", m.Runtime)
	}
	if m.SSH.Port != 22 || m.SSH.KnownHostsFile != homePath(".ssh/known_hosts") || m.SSH.InsecureSkipHostKeyVerify {
		t.Fatalf("ssh 기본값 = %+v", *m.SSH)
	}
	if m.SSH.KeyFile != homePath(".ssh/ci") {
		t.Fatalf("keyFile ~ 확장 = %q", m.SSH.KeyFile)
	}
}

// [§6.0, §9.1] sidecar 기본 이미지는 연결 머신의 공통 runtime 을 따른다.
func TestParse_SidecarDefaultImage_S9_1(t *testing.T) {
	cases := []struct {
		runtime string
		want    string
	}{
		// SPEC §9.1 의 문자열을 그대로 둔다. 상수가 잘못 바뀌면 여기서 잡힌다.
		{"docker", "docker:29.7.2-dind"},
		{"podman", "quay.io/podman/stable:v5.8.4"},
	}
	for _, c := range cases {
		y := strings.Replace(minimalYAML, "resources: { cpu: 2, memory: 4Gi }",
			"resources: { cpu: 2, memory: 4Gi }\n    jobRuntime: { mode: sidecar }", 1)
		y += "    runtime: " + c.runtime + "\n"
		cfg := mustParse(t, y, defaultEnv())
		if got := cfg.ScaleSets[0].SidecarImage; got != c.want {
			t.Fatalf("runtime %s: SidecarImage = %q, want %q", c.runtime, got, c.want)
		}
	}
	// 명시하면 그대로.
	y := strings.Replace(minimalYAML, "resources: { cpu: 2, memory: 4Gi }",
		"resources: { cpu: 2, memory: 4Gi }\n    jobRuntime: { mode: sidecar, image: docker:28.0.0-dind }", 1)
	cfg := mustParse(t, y, defaultEnv())
	if got := cfg.ScaleSets[0].SidecarImage; got != "docker:28.0.0-dind" {
		t.Fatalf("SidecarImage = %q", got)
	}
}

// ---------------------------------------------------------------------------
// R2 ~ R25 정적 규칙
// ---------------------------------------------------------------------------

// [R2] 알 수 없는 키는 strict decode 오류.
func TestParse_R2_UnknownKey(t *testing.T) {
	cases := map[string]string{
		"top":      minimalYAML + "version: 1\n",
		"nested":   strings.Replace(minimalYAML, "maxRunners: 4", "maxRunners: 4\n    maxRunner: 4", 1),
		"machine":  minimalYAML + "    hostname: x\n",
		"sshBlock": strings.Replace(sshYAML, "keyFile: ~/.ssh/ci", "keyFile: ~/.ssh/ci\n    fingerprint: SHA256:x", 1), // fingerprint 는 machines[].ssh 전용
	}
	for name, y := range cases {
		_, err := parse(t, y, defaultEnv())
		if err == nil {
			t.Fatalf("%s: 오류 기대", name)
		}
		wantRule(t, err, "R2")
	}
	// 두 번째 YAML 문서는 strict decode 를 피하는 통로가 되므로 문서 수 자체를 거부한다.
	_, err := parse(t, minimalYAML+"---\nversion: 1\n", defaultEnv())
	wantRule(t, err, "R2")
	_, err = parse(t, minimalYAML+"---\n", defaultEnv())
	wantRule(t, err, "R2")
}

// [R3] token 과 app 은 택1. app 은 세 필드 모두 필수.
func TestParse_R3_Auth(t *testing.T) {
	app := `
    app:
      clientId: Iv1.abc
      installationId: 123
      privateKey: "${file:~/app.pem}"`
	env := defaultEnv()
	env.files[homePath("app.pem")] = []byte("-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n")

	both := strings.Replace(minimalYAML, `token: "${env:GITHUB_PAT}"`, `token: "${env:GITHUB_PAT}"`+app, 1)
	_, err := parse(t, both, env)
	wantRule(t, err, "R3")

	none := strings.Replace(minimalYAML, `token: "${env:GITHUB_PAT}"`, `{}`, 1)
	_, err = parse(t, none, env)
	wantRule(t, err, "R3")

	appOnly := strings.Replace(minimalYAML, `token: "${env:GITHUB_PAT}"`, strings.TrimSpace(app), 1)
	cfg := mustParse(t, appOnly, env)
	a := cfg.GitHub.Auth.App
	if a == nil || a.ClientID != "Iv1.abc" || a.InstallationID != 123 || !strings.Contains(a.PrivateKey, "BEGIN PRIVATE KEY") {
		t.Fatalf("app = %+v", a)
	}

	for _, missing := range []string{"clientId: Iv1.abc", "installationId: 123", `privateKey: "${file:~/app.pem}"`} {
		y := strings.Replace(appOnly, missing, "", 1)
		_, err := parse(t, y, env)
		wantRule(t, err, "R3")
	}
}

// [R4] secret 필드의 리터럴은 오류이고, 오류 메시지에 리터럴 값이 새지 않는다.
func TestParse_R4_SecretLiteral(t *testing.T) {
	const literal = "ghp_LITERAL_SECRET"
	cases := map[string]string{
		"token":             strings.Replace(minimalYAML, `"${env:GITHUB_PAT}"`, literal, 1),
		"privateKey":        strings.Replace(minimalYAML, `token: "${env:GITHUB_PAT}"`, "app: { clientId: a, installationId: 1, privateKey: "+literal+" }", 1),
		"defaultPassphrase": strings.Replace(sshYAML, "keyFile: ~/.ssh/ci", "keyFile: ~/.ssh/ci\n    keyPassphrase: "+literal, 1),
		"machinePassphrase": sshYAML + "    ssh: { keyPassphrase: " + literal + " }\n",
	}
	for name, y := range cases {
		_, err := parse(t, y, defaultEnv())
		if err == nil {
			t.Fatalf("%s: 오류 기대", name)
		}
		wantRule(t, err, "R4")
		if strings.Contains(err.Error(), literal) {
			t.Fatalf("%s: 오류 메시지에 secret 리터럴이 노출됨: %v", name, err)
		}
	}
}

// [R5, §6.0] 참조 대상 없음(빈 env 포함)은 오류. 치환은 디코드 후 필드 값 단위.
func TestParse_R5_Reference(t *testing.T) {
	t.Run("env missing", func(t *testing.T) {
		env := defaultEnv()
		delete(env.env, "GITHUB_PAT")
		_, err := parse(t, minimalYAML, env)
		wantRule(t, err, "R5")
	})
	t.Run("env empty", func(t *testing.T) {
		env := defaultEnv()
		env.env["GITHUB_PAT"] = ""
		_, err := parse(t, minimalYAML, env)
		wantRule(t, err, "R5")
	})
	t.Run("file missing", func(t *testing.T) {
		y := strings.Replace(minimalYAML, "${env:GITHUB_PAT}", "${file:/etc/gh-ars/token}", 1)
		_, err := parse(t, y, defaultEnv())
		wantRule(t, err, "R5")
	})
	t.Run("file empty", func(t *testing.T) {
		y := strings.Replace(minimalYAML, "${env:GITHUB_PAT}", "${file:/etc/gh-ars/token}", 1)
		env := defaultEnv()
		env.files["/etc/gh-ars/token"] = []byte("  \n")
		_, err := parse(t, y, env)
		wantRule(t, err, "R5")
	})
	t.Run("file ok, whitespace trimmed, multi-line kept", func(t *testing.T) {
		y := strings.Replace(minimalYAML, `token: "${env:GITHUB_PAT}"`, `app: { clientId: a, installationId: 1, privateKey: "${file:~/app.pem}" }`, 1)
		env := defaultEnv()
		env.files[homePath("app.pem")] = []byte("-----BEGIN PRIVATE KEY-----\nline1\nline2\n-----END PRIVATE KEY-----\n")
		cfg := mustParse(t, y, env)
		got := cfg.GitHub.Auth.App.PrivateKey
		if strings.HasSuffix(got, "\n") || !strings.Contains(got, "line1\nline2") {
			t.Fatalf("privateKey 치환 결과 = %q", got)
		}
	})
	t.Run("cpu string reference substituted, numbers untouched", func(t *testing.T) {
		env := defaultEnv()
		env.env["UNIT_CPU"] = "0.5"
		y := strings.Replace(minimalYAML, "resources: { cpu: 2, memory: 4Gi }", `resources: { cpu: "${env:UNIT_CPU}", memory: 4Gi }`, 1)
		cfg := mustParse(t, y+"    resources: { cpu: 1.5, memory: 4Gi }\n", env)
		if cfg.ScaleSets[0].Unit.CPU != 0.5 || cfg.Machines[0].Resources.CPU != 1.5 {
			t.Fatalf("cpu = %v / %v", cfg.ScaleSets[0].Unit.CPU, cfg.Machines[0].Resources.CPU)
		}
		delete(env.env, "UNIT_CPU")
		_, err := parse(t, y, env)
		wantRule(t, err, "R5")
	})
	t.Run("non-secret field substituted too", func(t *testing.T) {
		y := strings.Replace(sshYAML, "host: 10.0.0.12", `host: "${env:BOX1_HOST}"`, 1)
		env := defaultEnv()
		env.env["BOX1_HOST"] = "10.9.9.9"
		cfg := mustParse(t, y, env)
		if cfg.Machines[0].SSH.Host != "10.9.9.9" {
			t.Fatalf("host = %q", cfg.Machines[0].SSH.Host)
		}
	})
}

// [R6, §3.1] github.url 로 org/repo 를 판별한다. enterprise 형태와 판별 불가는 오류.
func TestParse_R6_URLScope(t *testing.T) {
	cases := []struct {
		url         string
		scope       Scope
		owner, repo string
		wantErr     bool
	}{
		{"https://github.com/my-org", ScopeOrg, "my-org", "", false},
		{"https://github.com/my-org/", ScopeOrg, "my-org", "", false},
		{"https://github.com/my-org/my-repo", ScopeRepo, "my-org", "my-repo", false},
		{"https://ghes.example.com/my-org", ScopeOrg, "my-org", "", false},
		{"https://ghes.example.com/my-org/my-repo", ScopeRepo, "my-org", "my-repo", false},
		{"http://ghes.internal/my-org", ScopeOrg, "my-org", "", false},
		{"https://github.com/enterprises/acme", "", "", "", true},
		{"https://github.com", "", "", "", true},
		{"https://github.com/", "", "", "", true},
		{"https://github.com/a/b/c", "", "", "", true},
		{"https://github.com//b", "", "", "", true},
		{"github.com/my-org", "", "", "", true},
		{"ftp://github.com/my-org", "", "", "", true},
		{"https://github.com/my-org?x=1", "", "", "", true},
		{"", "", "", "", true},
	}
	for _, c := range cases {
		y := strings.Replace(minimalYAML, "url: https://github.com/my-org", "url: \""+c.url+"\"", 1)
		cfg, err := parse(t, y, defaultEnv())
		if c.wantErr {
			wantRule(t, err, "R6")
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", c.url, err)
		}
		gh := cfg.GitHub
		if gh.Scope != c.scope || gh.Owner != c.owner || gh.Repo != c.repo {
			t.Fatalf("%q: scope=%s owner=%s repo=%s", c.url, gh.Scope, gh.Owner, gh.Repo)
		}
		if gh.URL != c.url {
			t.Fatalf("%q: URL 은 원문을 유지해야 함: %q", c.url, gh.URL)
		}
	}
}

// [R7] repo scope 의 runnerGroup 은 Default 만(대소문자 무시). org 는 자유.
func TestParse_R7_RepoRunnerGroup(t *testing.T) {
	cases := []struct {
		url, group string
		wantErr    bool
	}{
		{"https://github.com/o/r", "Default", false},
		{"https://github.com/o/r", "default", false},
		{"https://github.com/o/r", "DEFAULT", false},
		{"https://github.com/o/r", "ci-group", true},
		{"https://github.com/o", "ci-group", false},
	}
	for _, c := range cases {
		y := strings.Replace(minimalYAML, "url: https://github.com/my-org", "url: "+c.url, 1)
		y = strings.Replace(y, "maxRunners: 4", "maxRunners: 4\n    runnerGroup: "+c.group, 1)
		cfg, err := parse(t, y, defaultEnv())
		if c.wantErr {
			wantRule(t, err, "R7")
			continue
		}
		if err != nil {
			t.Fatalf("%s/%s: %v", c.url, c.group, err)
		}
		if cfg.ScaleSets[0].RunnerGroup != c.group {
			t.Fatalf("runnerGroup 은 원문 유지: %q", cfg.ScaleSets[0].RunnerGroup)
		}
	}
}

// [R8] scaleSets[].name 중복.
func TestParse_R8_DuplicateScaleSet(t *testing.T) {
	y := strings.Replace(minimalYAML, "machines:", `  - name: linux-x64
    maxRunners: 1
    resources: { cpu: 1, memory: 1Gi }
machines:`, 1)
	_, err := parse(t, y, defaultEnv())
	wantRule(t, err, "R8")
}

// [R9] machines[].name 중복.
func TestParse_R9_DuplicateMachine(t *testing.T) {
	y := minimalYAML + "  - name: box1\n    scaleSet: linux-x64\n    host: 10.0.0.1\n"
	y = strings.Replace(y, "machines:", "machineDefaults: { ssh: { keyFile: ~/.ssh/ci } }\nmachines:", 1)
	_, err := parse(t, y, defaultEnv())
	wantRule(t, err, "R9")
}

// [R10] machines[].scaleSet 참조 대상 없음.
func TestParse_R10_UnknownScaleSetRef(t *testing.T) {
	y := strings.Replace(minimalYAML, "scaleSet: linux-x64", "scaleSet: nope", 1)
	_, err := parse(t, y, defaultEnv())
	wantRule(t, err, "R10")
}

// [R11] 연결 머신 0개인 scale set.
func TestParse_R11_ScaleSetWithoutMachine(t *testing.T) {
	y := strings.Replace(minimalYAML, "machines:", `  - name: lonely
    maxRunners: 1
    resources: { cpu: 1, memory: 1Gi }
machines:`, 1)
	_, err := parse(t, y, defaultEnv())
	wantRule(t, err, "R11")
}

// [R12] runtime 값은 docker/podman 만. machineDefaults 와 machines[] 양쪽.
func TestParse_R12_RuntimeValue(t *testing.T) {
	_, err := parse(t, minimalYAML+"    runtime: containerd\n", defaultEnv())
	wantRule(t, err, "R12")
	y := strings.Replace(minimalYAML, "machines:", "machineDefaults: { runtime: lxc }\nmachines:", 1)
	_, err = parse(t, y, defaultEnv())
	wantRule(t, err, "R12")

	cfg := mustParse(t, minimalYAML+"    runtime: podman\n", defaultEnv())
	if cfg.Machines[0].Runtime != domain.RuntimePodman {
		t.Fatalf("runtime = %q", cfg.Machines[0].Runtime)
	}
	y = strings.Replace(minimalYAML, "machines:", "machineDefaults: { runtime: podman }\nmachines:", 1)
	cfg = mustParse(t, y, defaultEnv())
	if cfg.Machines[0].Runtime != domain.RuntimePodman {
		t.Fatalf("machineDefaults.runtime 상속 실패: %q", cfg.Machines[0].Runtime)
	}
}

// [R13] runner.image / jobRuntime.image 의 :latest 또는 태그 없음은 오류. 다이제스트 참조는 허용.
func TestParse_R13_ImageTag(t *testing.T) {
	cases := []struct {
		image   string
		wantErr bool
	}{
		{"ghcr.io/actions/actions-runner:2.337.0", false},
		{"ghcr.io/actions/actions-runner:latest", true},
		{"ghcr.io/actions/actions-runner", true},
		{"actions-runner", true},
		{"actions-runner:latest", true},
		{"localhost:5000/runner", true},                                      // 포트는 태그가 아니다
		{"localhost:5000/runner:v1", false},                                  // 포트 + 태그
		{"localhost:5000/runner:latest", true},                               // 포트 + latest
		{"ghcr.io/x/runner@sha256:" + strings.Repeat("a", 64), false},        // 다이제스트만
		{"ghcr.io/x/runner:latest@sha256:" + strings.Repeat("a", 64), false}, // 다이제스트가 있으면 불변
		{"ghcr.io/x/runner@sha512:" + strings.Repeat("0", 128), false},
		{"ghcr.io/x/runner@oops", true},                              // "@" 만으로는 예외가 아니다
		{"ghcr.io/x/runner@sha256:" + strings.Repeat("a", 63), true}, // 길이 불일치
		{"ghcr.io/x/runner@sha256:" + strings.Repeat("G", 64), true}, // hex 아님
		{"ghcr.io/x/runner@md5:" + strings.Repeat("a", 32), true},    // 미등록 알고리즘
	}
	for _, c := range cases {
		y := strings.Replace(minimalYAML, "maxRunners: 4", "maxRunners: 4\n    runner: { image: \""+c.image+"\" }", 1)
		_, err := parse(t, y, defaultEnv())
		if c.wantErr {
			wantRule(t, err, "R13")
		} else if err != nil {
			t.Fatalf("runner.image %q: %v", c.image, err)
		}
		// jobRuntime.image 도 같은 규칙(mode 와 무관하게 값이 있으면 검사).
		y = strings.Replace(minimalYAML, "maxRunners: 4", "maxRunners: 4\n    jobRuntime: { image: \""+c.image+"\" }", 1)
		_, err = parse(t, y, defaultEnv())
		if c.wantErr {
			wantRule(t, err, "R13")
		} else if err != nil {
			t.Fatalf("jobRuntime.image %q: %v", c.image, err)
		}
	}
}

// [R14, §4.3] len(scaleSet)+len(machine) > 36 이면 runner 이름이 64자를 넘는다.
func TestParse_R14_RunnerNameLength(t *testing.T) {
	ss := strings.Repeat("s", 20)
	ok := strings.Repeat("m", 16)  // 36
	bad := strings.Repeat("m", 17) // 37
	for _, c := range []struct {
		machine string
		wantErr bool
	}{{ok, false}, {bad, true}} {
		y := strings.ReplaceAll(minimalYAML, "linux-x64", ss)
		y = strings.Replace(y, "name: box1", "name: "+c.machine, 1)
		cfg, err := parse(t, y, defaultEnv())
		if c.wantErr {
			wantRule(t, err, "R14")
			continue
		}
		if err != nil {
			t.Fatalf("len 36: %v", err)
		}
		name := domain.RunnerName(cfg.ScaleSets[0].Name, cfg.Machines[0].Name, domain.UnitID(strings.Repeat("0", domain.UnitIDLen)))
		if len(name) != domain.MaxRunnerNameLen {
			t.Fatalf("경계 runner 이름 길이 = %d", len(name))
		}
	}
}

// [R15] sidecar scale set 의 머신 runtime 은 모두 같아야 한다. none 은 혼합 허용.
func TestParse_R15_SidecarRuntimeMismatch(t *testing.T) {
	two := strings.Replace(sshYAML, "host: 10.0.0.12", "host: 10.0.0.12\n    runtime: docker", 1) +
		"  - name: box2\n    scaleSet: linux-x64\n    host: 10.0.0.13\n    runtime: podman\n"
	cfg := mustParse(t, two, defaultEnv())
	if got := cfg.ScaleSets[0].Machines; len(got) != 2 {
		t.Fatalf("none 혼합: %v", got)
	}
	sidecar := strings.Replace(two, "resources: { cpu: 2, memory: 4Gi }", "resources: { cpu: 2, memory: 4Gi }\n    jobRuntime: { mode: sidecar }", 1)
	_, err := parse(t, sidecar, defaultEnv())
	wantRule(t, err, "R15")
	// 같은 runtime 이면 통과.
	same := strings.Replace(sidecar, "runtime: docker", "runtime: podman", 1)
	cfg = mustParse(t, same, defaultEnv())
	if cfg.ScaleSets[0].SidecarImage != DefaultSidecarImage[domain.RuntimePodman] {
		t.Fatalf("SidecarImage = %q", cfg.ScaleSets[0].SidecarImage)
	}
}

// [§6.0] jobRuntime.mode 는 none/sidecar 만.
func TestParse_ModeValue_S6_0(t *testing.T) {
	y := strings.Replace(minimalYAML, "maxRunners: 4", "maxRunners: 4\n    jobRuntime: { mode: dind }", 1)
	_, err := parse(t, y, defaultEnv())
	wantRule(t, err, "mode")
}

// [R17] host 없는 머신에 ssh 블록.
func TestParse_R17_LocalWithSSH(t *testing.T) {
	_, err := parse(t, minimalYAML+"    ssh: { user: x }\n", defaultEnv())
	wantRule(t, err, "R17")
	_, err = parse(t, minimalYAML+"    ssh: {}\n", defaultEnv())
	wantRule(t, err, "R17")
}

// [R18] host 없는 머신은 설정 파일당 1개.
func TestParse_R18_TwoLocalMachines(t *testing.T) {
	_, err := parse(t, minimalYAML+"  - name: box2\n    scaleSet: linux-x64\n", defaultEnv())
	wantRule(t, err, "R18")
}

// [R19] keyFile 필수(SSH 머신 존재 시), keyPassphrase 유무와 키 암호화 여부 일치.
func TestParse_R19_KeyFileAndPassphrase(t *testing.T) {
	t.Run("keyFile missing", func(t *testing.T) {
		y := strings.Replace(sshYAML, "    keyFile: ~/.ssh/ci\n", "", 1)
		_, err := parse(t, y, defaultEnv())
		wantRule(t, err, "R19")
	})
	t.Run("keyFile unreadable", func(t *testing.T) {
		env := defaultEnv()
		delete(env.files, homePath(".ssh/ci"))
		_, err := parse(t, sshYAML, env)
		wantRule(t, err, "R19")
	})
	t.Run("local only: keyFile not required", func(t *testing.T) {
		mustParse(t, minimalYAML, defaultEnv())
	})
	withPass := strings.Replace(sshYAML, "keyFile: ~/.ssh/ci", "keyFile: ~/.ssh/ci\n    keyPassphrase: \"${env:CI_KEY_PASS}\"", 1)
	keys := map[string]struct {
		data      []byte
		encrypted bool
	}{
		"openssh plain":     {opensshKey("none", "none"), false},
		"openssh encrypted": {opensshKey("aes256-ctr", "bcrypt"), true},
		"pkcs1 plain":       {pemKey("RSA PRIVATE KEY", nil), false},
		"pkcs1 encrypted":   {pemKey("RSA PRIVATE KEY", map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,00"}), true},
		"pkcs8 plain":       {pemKey("PRIVATE KEY", nil), false},
		"pkcs8 encrypted":   {pemKey("ENCRYPTED PRIVATE KEY", nil), true},
	}
	for name, k := range keys {
		for _, hasPass := range []bool{false, true} {
			env := defaultEnv()
			env.env["CI_KEY_PASS"] = "pass"
			env.files[homePath(".ssh/ci")] = k.data
			y := sshYAML
			if hasPass {
				y = withPass
			}
			cfg, err := parse(t, y, env)
			if hasPass != k.encrypted {
				wantRule(t, err, "R19")
				continue
			}
			if err != nil {
				t.Fatalf("%s pass=%v: %v", name, hasPass, err)
			}
			if got := cfg.Machines[0].SSH.KeyPassphrase; hasPass && got != "pass" || !hasPass && got != "" {
				t.Fatalf("%s: keyPassphrase = %q", name, got)
			}
		}
	}
	t.Run("not a key", func(t *testing.T) {
		env := defaultEnv()
		env.files[homePath(".ssh/ci")] = []byte("not pem")
		_, err := parse(t, sshYAML, env)
		wantRule(t, err, "R19")
	})
	t.Run("per-machine override", func(t *testing.T) {
		env := defaultEnv()
		env.env["CI_KEY_PASS"] = "pass"
		env.files["/keys/enc"] = opensshKey("aes256-ctr", "bcrypt")
		// 기본은 평문 키, box1 만 암호화 키 + passphrase.
		y := sshYAML + "    ssh: { keyFile: /keys/enc, keyPassphrase: \"${env:CI_KEY_PASS}\" }\n"
		cfg := mustParse(t, y, env)
		if s := cfg.Machines[0].SSH; s.KeyFile != "/keys/enc" || s.KeyPassphrase != "pass" || s.User != "runner" {
			t.Fatalf("병합 결과 = %+v", *s)
		}
		// 기본 passphrase 를 머신에서 빈 값으로 덮을 수는 없다(빈 값 = 미지정). 평문 키 + 기본 passphrase 는 R19.
		y = withPass + "    ssh: { keyFile: /keys/plain }\n"
		env.files["/keys/plain"] = opensshKey("none", "none")
		_, err := parse(t, y, env)
		wantRule(t, err, "R19")
	})
}

// [R20] host key 검증 재료가 전혀 없으면 오류. insecureSkipHostKeyVerify 는 경고.
func TestParse_R20_HostKey(t *testing.T) {
	t.Run("insecure warns", func(t *testing.T) {
		y := strings.Replace(sshYAML, "keyFile: ~/.ssh/ci", "keyFile: ~/.ssh/ci\n    insecureSkipHostKeyVerify: true", 1)
		cfg := mustParse(t, y, defaultEnv())
		if !cfg.Machines[0].SSH.InsecureSkipHostKeyVerify {
			t.Fatal("insecure 상속 실패")
		}
		if len(cfg.Warnings) != 1 || !strings.HasPrefix(cfg.Warnings[0], "R20") {
			t.Fatalf("경고 = %v", cfg.Warnings)
		}
		// 머신에서 false 로 덮으면 경고 없음.
		y2 := y + "    ssh: { insecureSkipHostKeyVerify: false }\n"
		cfg = mustParse(t, y2, defaultEnv())
		if cfg.Machines[0].SSH.InsecureSkipHostKeyVerify || len(cfg.Warnings) != 0 {
			t.Fatalf("override 실패: %+v %v", *cfg.Machines[0].SSH, cfg.Warnings)
		}
	})
	t.Run("no fingerprint, empty knownHostsFile", func(t *testing.T) {
		y := strings.Replace(sshYAML, "keyFile: ~/.ssh/ci", "keyFile: ~/.ssh/ci\n    knownHostsFile: \"\"", 1)
		_, err := parse(t, y, defaultEnv())
		wantRule(t, err, "R20")
		// fingerprint 가 있으면 통과.
		cfg := mustParse(t, y+"    ssh: { fingerprint: \"SHA256:abc\" }\n", defaultEnv())
		if s := cfg.Machines[0].SSH; s.Fingerprint != "SHA256:abc" || s.KnownHostsFile != "" {
			t.Fatalf("ssh = %+v", *s)
		}
		// insecure 면 통과(경고).
		cfg = mustParse(t, y+"    ssh: { insecureSkipHostKeyVerify: true }\n", defaultEnv())
		if len(cfg.Warnings) != 1 {
			t.Fatalf("경고 = %v", cfg.Warnings)
		}
	})
}

// [R23] scaleSet.maxRunners 는 필수. (> Σ effectiveMax cap 은 plan 의 동적 규칙)
func TestParse_R23_MaxRunnersRequired(t *testing.T) {
	y := strings.Replace(minimalYAML, "    maxRunners: 4\n", "", 1)
	_, err := parse(t, y, defaultEnv())
	wantRule(t, err, "R23")
	// 0 은 허용(listener 도 0 을 받는다. DESIGN §4.5). 음수는 오류.
	mustParse(t, strings.Replace(minimalYAML, "maxRunners: 4", "maxRunners: 0", 1), defaultEnv())
	_, err = parse(t, strings.Replace(minimalYAML, "maxRunners: 4", "maxRunners: -1", 1), defaultEnv())
	wantRule(t, err, "range")
}

// [§6.0] 음수 개수는 오류: minRunners, machines[].maxRunners, ssh.port.
func TestParse_NegativeCounts_S6_0(t *testing.T) {
	_, err := parse(t, strings.Replace(minimalYAML, "maxRunners: 4", "maxRunners: 4\n    minRunners: -1", 1), defaultEnv())
	wantRule(t, err, "range")
	_, err = parse(t, minimalYAML+"    maxRunners: -2\n", defaultEnv())
	wantRule(t, err, "range")
	_, err = parse(t, strings.Replace(sshYAML, "user: runner", "user: runner\n    port: 0", 1), defaultEnv())
	wantRule(t, err, "range")
}

// [R25] cpu/memory 파싱 실패와 누락. machines[].resources 는 블록 단위로 둘 다 필요.
func TestParse_R25_Resources(t *testing.T) {
	bad := []string{
		"{ cpu: 0, memory: 4Gi }",
		"{ cpu: -1, memory: 4Gi }",
		"{ cpu: two, memory: 4Gi }",
		"{ cpu: 0.001, memory: 4Gi }",
		"{ cpu: 2, memory: 4G }",
		"{ cpu: 2, memory: 1.5Gi }",
		"{ cpu: 2, memory: 4096 }",
		"{ cpu: 2 }",
		"{ memory: 4Gi }",
		"{}",
	}
	for _, r := range bad {
		y := strings.Replace(minimalYAML, "resources: { cpu: 2, memory: 4Gi }", "resources: "+r, 1)
		_, err := parse(t, y, defaultEnv())
		wantRule(t, err, "R25")
		y = minimalYAML + "    resources: " + r + "\n"
		_, err = parse(t, y, defaultEnv())
		wantRule(t, err, "R25")
	}
	// cpu 와 memory 가 둘 다 틀리면 오류 2개를 모두 보고한다 (DESIGN §8).
	y := strings.Replace(minimalYAML, "resources: { cpu: 2, memory: 4Gi }", "resources: { cpu: 0, memory: 4G }", 1)
	_, err := parse(t, y, defaultEnv())
	if n := len(Rules(err)); n != 2 {
		t.Fatalf("R25 오류 수 = %d, want 2: %v", n, err)
	}
	// resources 블록 자체가 없으면 scale set 은 R25 오류, 머신은 허용(자동 탐지).
	y = strings.Replace(minimalYAML, "    resources: { cpu: 2, memory: 4Gi }\n", "", 1)
	_, err = parse(t, y, defaultEnv())
	wantRule(t, err, "R25")

	good := []struct {
		r        string
		cpu      float64
		memBytes int64
	}{
		{"{ cpu: 0.5, memory: 512Mi }", 0.5, 512 << 20},
		{"{ cpu: \"2\", memory: 1024Ki }", 2, 1024 << 10},
		{"{ cpu: 0.125, memory: 1536Mi }", 0.125, 1536 << 20},
	}
	for _, g := range good {
		y := strings.Replace(minimalYAML, "resources: { cpu: 2, memory: 4Gi }", "resources: "+g.r, 1)
		cfg := mustParse(t, y+"    resources: "+g.r+"\n", defaultEnv())
		if u := cfg.ScaleSets[0].Unit; u.CPU != g.cpu || u.MemoryBytes != g.memBytes {
			t.Fatalf("%s: Unit = %+v", g.r, u)
		}
		if m := cfg.Machines[0].Resources; m == nil || m.CPU != g.cpu || m.MemoryBytes != g.memBytes {
			t.Fatalf("%s: machine Resources = %+v", g.r, m)
		}
	}
}

// [§6.0] 필수 필드 누락: scaleSets[].name, machines[].name, machines[].scaleSet, scaleSets/machines 목록 자체.
func TestParse_RequiredFields_S6_0(t *testing.T) {
	cases := map[string]string{
		"scaleSet name": strings.Replace(minimalYAML, "  - name: linux-x64\n    maxRunners: 4\n", "  - maxRunners: 4\n", 1),
		"machine name":  strings.Replace(minimalYAML, "  - name: box1\n", "  - host: 1.2.3.4\n", 1),
		"machine ss":    strings.Replace(minimalYAML, "    scaleSet: linux-x64\n", "", 1),
		"no scaleSets":  "github:\n  url: https://github.com/o\n  auth:\n    token: \"${env:GITHUB_PAT}\"\nmachines:\n  - name: box1\n    scaleSet: x\n",
		"no machines":   "github:\n  url: https://github.com/o\n  auth:\n    token: \"${env:GITHUB_PAT}\"\nscaleSets:\n  - name: x\n    maxRunners: 1\n    resources: { cpu: 1, memory: 1Gi }\n",
	}
	for name, y := range cases {
		_, err := parse(t, y, defaultEnv())
		if err == nil {
			t.Fatalf("%s: 오류 기대", name)
		}
		wantRule(t, err, "required")
	}
}

// [DESIGN §8] 검증 오류는 전부 모아 한 번에 보고한다.
func TestParse_CollectsAllErrors(t *testing.T) {
	y := strings.Replace(minimalYAML, "url: https://github.com/my-org", "url: https://github.com/enterprises/x", 1)
	y = strings.Replace(y, "maxRunners: 4", "runner: { image: r:latest }", 1)
	y = strings.Replace(y, "scaleSet: linux-x64", "scaleSet: nope\n    runtime: lxc", 1)
	_, err := parse(t, y, defaultEnv())
	for _, r := range []string{"R6", "R13", "R23", "R10", "R12", "R11"} {
		wantRule(t, err, r)
	}
	var re *RuleError
	if !errors.As(err, &re) {
		t.Fatalf("RuleError 로 언랩 불가: %v", err)
	}
	wantNoRule(t, err, "R2")
}

// Config.ScaleSet 은 scaleset delete 가 runnerGroup 을 찾을 때 쓴다 (§11).
func TestConfig_ScaleSetLookup_S11(t *testing.T) {
	cfg := mustParse(t, minimalYAML, defaultEnv())
	if ss, ok := cfg.ScaleSet("linux-x64"); !ok || ss.RunnerGroup != "Default" {
		t.Fatalf("lookup = %+v %v", ss, ok)
	}
	if _, ok := cfg.ScaleSet("nope"); ok {
		t.Fatal("없는 이름이 찾아짐")
	}
}

// Load 는 파일을 읽어 OS env 로 Parse 한다. 존재하지 않는 파일은 오류.
func TestLoad_MissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("오류 기대")
	}
}

// RuleError 문자열 형식: "R# path: msg" / "R#: msg".
func TestRuleError_Format(t *testing.T) {
	e := &RuleError{Rule: "R13", Path: "scaleSets[0].runner.image", Msg: "tag"}
	if got := e.Error(); got != "R13 scaleSets[0].runner.image: tag" {
		t.Fatalf("Error() = %q", got)
	}
	e = &RuleError{Rule: "R18", Msg: "two"}
	if got := e.Error(); got != "R18: two" {
		t.Fatalf("Error() = %q", got)
	}
}
