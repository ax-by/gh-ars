package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"gh-ars/internal/domain"
	"gh-ars/internal/executor"
	"gh-ars/internal/runtime"
)

// fakeExec 는 executor.Executor 대역이다. 실제 runtime 코드(argv 조립·출력 파싱)를 그대로
// 태우고, 명령마다 스크립트한 결과를 돌려준다. 기록은 `sudo -n ` 접두를 포함한 argv 문자열이다.
type fakeExec struct {
	mu     sync.Mutex
	log    []string
	closed int
	// h 는 sudo 접두 여부와 argv 로 결과를 정한다. nil 결과는 종료 코드 0 + 빈 출력.
	h func(sudo bool, argv []string) (executor.Result, error)
	// stream 은 Stream 이 돌려줄 내용이다. 비어 있으면 즉시 EOF(스트림 종료).
	stream string
	// budget 은 명령마다 남아 있던 ctx 여유다(상한이 없으면 항목이 없다). [§8.3 상수 표]
	budget map[string]time.Duration
	// holdAfter 는 이 번째 이후의 Stream 이 ctx 취소까지 열린 채로 있게 한다(0 이면 전부 즉시 EOF).
	// 회차 하나가 Resynced 까지 가는 것을 재현하는 데 쓴다.
	holdAfter int
	streamN   int
}

// holdReader 는 ctx 가 끝날 때까지 열려 있는 스트림이다(실제 executor 의 Stream 과 같은 계약).
type holdReader struct{ done <-chan struct{} }

func (h holdReader) Read([]byte) (int, error) { <-h.done; return 0, io.EOF }
func (h holdReader) Close() error             { return nil }

func (f *fakeExec) note(key string, ctx context.Context) {
	dl, ok := ctx.Deadline()
	if !ok {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.budget == nil {
		f.budget = map[string]time.Duration{}
	}
	if _, seen := f.budget[key]; !seen {
		f.budget[key] = time.Until(dl)
	}
}

// budgetFor 는 argv 가 prefix 로 시작한 첫 명령의 ctx 여유다.
func (f *fakeExec) budgetFor(prefix string) (time.Duration, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range f.budget {
		if strings.HasPrefix(k, prefix) {
			return v, true
		}
	}
	return 0, false
}

func cmdKey(c executor.Cmd) string {
	s := strings.Join(c.Argv, " ")
	if c.Sudo {
		return "sudo -n " + s
	}
	return s
}

func (f *fakeExec) Run(ctx context.Context, c executor.Cmd) (executor.Result, error) {
	f.note(cmdKey(c), ctx)
	f.mu.Lock()
	f.log = append(f.log, cmdKey(c))
	f.mu.Unlock()
	if f.h == nil {
		return executor.Result{}, nil
	}
	return f.h(c.Sudo, c.Argv)
}

func (f *fakeExec) Stream(ctx context.Context, c executor.Cmd) (io.ReadCloser, error) {
	f.mu.Lock()
	f.log = append(f.log, cmdKey(c))
	f.streamN++
	hold := f.holdAfter > 0 && f.streamN > f.holdAfter
	f.mu.Unlock()
	if hold {
		return holdReader{ctx.Done()}, nil
	}
	return io.NopCloser(strings.NewReader(f.stream)), nil
}

func (f *fakeExec) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeExec) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *fakeExec) sawSudo() bool {
	for _, c := range f.calls() {
		if strings.HasPrefix(c, "sudo -n ") {
			return true
		}
	}
	return false
}

// --- 출력 대역 ---

func dockerInfoJSON(driver, version string) string {
	return fmt.Sprintf(`{"NCPU":8,"MemTotal":%d,"CgroupDriver":%q,"CgroupVersion":%q}`, 16<<30, driver, version)
}

func podmanInfoJSON(rootless bool, manager, version string) string {
	return fmt.Sprintf(`{"host":{"cgroupManager":%q,"cgroupVersion":%q,"cpus":8,"memTotal":%d,"security":{"rootless":%t}}}`,
		manager, version, int64(16)<<30, rootless)
}

func ok(stdout string) (executor.Result, error) {
	return executor.Result{Stdout: []byte(stdout)}, nil
}

func fail(stderr string) (executor.Result, error) {
	return executor.Result{Stderr: []byte(stderr), ExitCode: 1}, nil
}

// isCmd 는 argv 가 주어진 토큰들로 시작하는지다("podman", "info").
func isCmd(argv []string, want ...string) bool {
	if len(argv) < len(want) {
		return false
	}
	for i, w := range want {
		if argv[i] != w {
			return false
		}
	}
	return true
}

// uid 는 `id -u` 응답이다. 그 밖의 명령은 next 에 넘긴다.
func uid(u string, next func(bool, []string) (executor.Result, error)) func(bool, []string) (executor.Result, error) {
	return func(sudo bool, argv []string) (executor.Result, error) {
		if isCmd(argv, "id", "-u") {
			return ok(u + "\n")
		}
		if next == nil {
			return executor.Result{}, nil
		}
		return next(sudo, argv)
	}
}

// fakeSlices 는 SliceChecker 대역이다. NewSlices 가 받은 sudo 를 기록한다. [§10.2 규칙 4]
type fakeSlices struct {
	called *int
	err    error
}

func (f fakeSlices) Check(context.Context) error {
	*f.called++
	return f.err
}

// noSlices 는 통과용 SliceChecker 다. sidecar 의 다른 R16 분기를 검증할 때 쓴다.
func noSlices(executor.Executor, bool) SliceChecker { return fakeSlices{called: new(int)} }

func newAgent(t *testing.T, spec Spec, ex *fakeExec) *Agent {
	t.Helper()
	spec.NewExecutor = func() (executor.Executor, error) { return ex, nil }
	a, err := New(spec, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// TestPreflight_S10_2_DockerNoSudo: 규칙 2 — docker 는 어떤 명령에도 sudo 를 붙이지 않는다.
// pre-pull(§7.1-4)도 같은 경로로 나간다. [§10.2 규칙 2, §7.1-4]
func TestPreflight_S10_2_DockerNoSudo(t *testing.T) {
	ex := &fakeExec{h: uid("1000", func(_ bool, argv []string) (executor.Result, error) {
		if isCmd(argv, "docker", "info") {
			return ok(dockerInfoJSON("systemd", "2"))
		}
		return executor.Result{}, nil
	})}
	a := newAgent(t, Spec{Name: "m1", Local: true, Runtime: domain.RuntimeDocker, Mode: domain.ModeNone,
		Images: []string{"ghcr.io/actions/actions-runner:2.337.0"}}, ex)

	info, err := a.Preflight(context.Background())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if info.CPUs != 8 || info.MemoryBytes != 16<<30 {
		t.Fatalf("info = %+v", info)
	}
	if ex.sawSudo() {
		t.Fatalf("docker 경로에 sudo 가 붙었다: %v", ex.calls())
	}
	if got := ex.calls(); len(got) != 3 || got[0] != "id -u" ||
		!strings.HasPrefix(got[1], "docker info") || !strings.HasPrefix(got[2], "docker pull ") {
		t.Fatalf("preflight 순서 = %v, want id -u → info → pull", got)
	}
}

// TestPreflight_S10_2_DockerInfoFailure: `docker info` 실패는 **모드와 무관하게** unhealthy 다.
// 데몬이 잠시 죽은 것을 R16(설정·환경 모순)으로 올리면 재접속 회차에서 그 머신이 Failed 로
// 영구 제외되어 살아나지 못한다. R16 은 rootless·cgroup·권한 같은 환경 모순에만 쓴다. [§10.2 규칙 2, §9.2]
func TestPreflight_S10_2_DockerInfoFailure(t *testing.T) {
	for _, tc := range []struct {
		mode  domain.Mode
		fatal bool
	}{
		{domain.ModeNone, false},
		{domain.ModeSidecar, false},
	} {
		ex := &fakeExec{h: uid("0", func(_ bool, argv []string) (executor.Result, error) {
			if isCmd(argv, "docker", "info") {
				return fail("Cannot connect to the Docker daemon")
			}
			return executor.Result{}, nil
		})}
		a := newAgent(t, Spec{Name: "m1", Local: true, Runtime: domain.RuntimeDocker, Mode: tc.mode, NewSlices: noSlices}, ex)
		_, err := a.Preflight(context.Background())
		if err == nil {
			t.Fatalf("mode=%s: 오류 없음", tc.mode)
		}
		if got := errors.Is(err, ErrFatal); got != tc.fatal {
			t.Fatalf("mode=%s: ErrFatal=%v, want %v (err=%v)", tc.mode, got, tc.fatal, err)
		}
	}
}

// TestPreflight_S10_2_PodmanSudoPathFixed: 규칙 3 — `podman info` 가 실패하면 `sudo -n podman info`
// 를 시도하고, 성공한 경로를 이후 **모든** podman 명령에 고정한다. [§10.2 규칙 3]
func TestPreflight_S10_2_PodmanSudoPathFixed(t *testing.T) {
	ex := &fakeExec{h: uid("1000", func(sudo bool, argv []string) (executor.Result, error) {
		if isCmd(argv, "podman", "info") {
			if !sudo {
				return fail("Cannot connect to Podman socket")
			}
			return ok(podmanInfoJSON(false, "systemd", "v2"))
		}
		return executor.Result{}, nil
	})}
	a := newAgent(t, Spec{Name: "m1", Local: true, Runtime: domain.RuntimePodman, Mode: domain.ModeNone,
		Images: []string{"img"}}, ex)

	info, err := a.Preflight(context.Background())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if info.CgroupVersion != "2" { // podman 의 v2 정규화 [DESIGN §4.2]
		t.Fatalf("CgroupVersion = %q", info.CgroupVersion)
	}
	calls := ex.calls()
	if len(calls) != 4 || !strings.HasPrefix(calls[1], "podman info") || !strings.HasPrefix(calls[2], "sudo -n podman info") {
		t.Fatalf("규칙 3 시도 순서 = %v", calls)
	}
	if !strings.HasPrefix(calls[3], "sudo -n podman pull ") {
		t.Fatalf("고정된 경로가 pre-pull 에 전파되지 않았다: %v", calls)
	}
	// 고정 결과는 Runtime 에도 남는다: 이후 명령(Observe)도 sudo 경로다.
	if _, err := a.Observe(context.Background()); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	for _, c := range ex.calls()[4:] {
		if !strings.HasPrefix(c, "sudo -n podman ") {
			t.Fatalf("고정 경로 밖의 명령: %q", c)
		}
	}
}

// TestPreflight_S10_2_PodmanBothPathsFail: 두 경로 모두 실패면 모드와 무관하게 unhealthy 다
// (환경 모순이 아니라 도달 불가이므로 재접속에서 다시 본다). [§10.2 규칙 3, §9.2]
func TestPreflight_S10_2_PodmanBothPathsFail(t *testing.T) {
	for _, tc := range []struct {
		mode  domain.Mode
		fatal bool
	}{
		{domain.ModeNone, false},
		{domain.ModeSidecar, false},
	} {
		ex := &fakeExec{h: uid("1000", func(_ bool, argv []string) (executor.Result, error) {
			if isCmd(argv, "podman", "info") {
				return fail("permission denied")
			}
			return executor.Result{}, nil
		})}
		a := newAgent(t, Spec{Name: "m1", Local: true, Runtime: domain.RuntimePodman, Mode: tc.mode, NewSlices: noSlices}, ex)
		_, err := a.Preflight(context.Background())
		if err == nil {
			t.Fatalf("mode=%s: 오류 없음", tc.mode)
		}
		if got := errors.Is(err, ErrFatal); got != tc.fatal {
			t.Fatalf("mode=%s: ErrFatal=%v, want %v (err=%v)", tc.mode, got, tc.fatal, err)
		}
	}
}

// TestPreflight_S10_2_PodmanRootlessNoneAllowed: none 은 어느 경로든 성공하면 healthy(rootless 허용).
// sudo 경로는 시도하지 않는다. [§10.2 규칙 3, R16]
func TestPreflight_S10_2_PodmanRootlessNoneAllowed(t *testing.T) {
	ex := &fakeExec{h: uid("1000", func(sudo bool, argv []string) (executor.Result, error) {
		if isCmd(argv, "podman", "info") {
			if sudo {
				t.Errorf("none 모드인데 sudo 경로를 시도했다")
			}
			return ok(podmanInfoJSON(true, "systemd", "v2"))
		}
		return executor.Result{}, nil
	})}
	a := newAgent(t, Spec{Name: "m1", Local: true, Runtime: domain.RuntimePodman, Mode: domain.ModeNone}, ex)
	info, err := a.Preflight(context.Background())
	if err != nil {
		t.Fatalf("rootless podman + none 은 허용된다: %v", err)
	}
	if !info.Rootless {
		t.Fatalf("Rootless 가 전달되지 않았다: %+v", info)
	}
}

// TestPreflight_R16_PodmanRootlessSidecar: sidecar 에서 고정 경로가 rootless 면 아직 시도하지 않은
// `sudo -n podman info` 로 rootful 을 얻어 경로를 바꾸고, 그래도 rootless 면 R16 오류. [§10.2 규칙 3, R16]
func TestPreflight_R16_PodmanRootlessSidecar(t *testing.T) {
	t.Run("sudo 로 rootful 획득", func(t *testing.T) {
		sudoUsed := false
		ex := &fakeExec{h: uid("1000", func(sudo bool, argv []string) (executor.Result, error) {
			if isCmd(argv, "podman", "info") {
				if sudo {
					sudoUsed = true
					return ok(podmanInfoJSON(false, "systemd", "v2"))
				}
				return ok(podmanInfoJSON(true, "systemd", "v2"))
			}
			return executor.Result{}, nil
		})}
		calledN, sudoSeen := 0, false
		a := newAgent(t, Spec{Name: "m1", Local: true, Runtime: domain.RuntimePodman, Mode: domain.ModeSidecar,
			Images: []string{"img"},
			NewSlices: func(_ executor.Executor, sudo bool) SliceChecker {
				sudoSeen = sudo
				return fakeSlices{called: &calledN}
			}}, ex)
		info, err := a.Preflight(context.Background())
		if err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		if !sudoUsed || info.Rootless {
			t.Fatalf("rootful 경로로 바뀌지 않았다: sudoUsed=%v info=%+v", sudoUsed, info)
		}
		if calledN != 1 || !sudoSeen {
			t.Fatalf("slice 확인 호출=%d sudo=%v, want 1/true (id -u=1000 → 규칙 4)", calledN, sudoSeen)
		}
		last := ex.calls()[len(ex.calls())-1]
		if !strings.HasPrefix(last, "sudo -n podman pull ") {
			t.Fatalf("바뀐 경로가 전파되지 않았다: %q", last)
		}
	})

	t.Run("여전히 rootless 면 R16", func(t *testing.T) {
		ex := &fakeExec{h: uid("1000", func(_ bool, argv []string) (executor.Result, error) {
			if isCmd(argv, "podman", "info") {
				return ok(podmanInfoJSON(true, "systemd", "v2"))
			}
			return executor.Result{}, nil
		})}
		a := newAgent(t, Spec{Name: "m1", Local: true, Runtime: domain.RuntimePodman, Mode: domain.ModeSidecar,
			NewSlices: func(executor.Executor, bool) SliceChecker { return fakeSlices{called: new(int)} }}, ex)
		_, err := a.Preflight(context.Background())
		if !errors.Is(err, ErrFatal) {
			t.Fatalf("rootless podman + sidecar 는 R16 오류여야 한다: %v", err)
		}
	})
}

// TestPreflight_R16_RootlessDocker: sidecar 전제는 rootful runtime 이다(§9.2). podman 은 경로
// 고정에서 걸리지만 docker 는 여기가 유일한 판정 지점이다. [R16, §9.2]
func TestPreflight_R16_RootlessDocker(t *testing.T) {
	ex := &fakeExec{h: uid("1000", func(_ bool, argv []string) (executor.Result, error) {
		if isCmd(argv, "docker", "info") {
			return ok(`{"NCPU":8,"MemTotal":17179869184,"CgroupDriver":"systemd","CgroupVersion":"2","SecurityOptions":["name=rootless"]}`)
		}
		return executor.Result{}, nil
	})}
	a := newAgent(t, Spec{Name: "m1", Local: true, Runtime: domain.RuntimeDocker, Mode: domain.ModeSidecar,
		NewSlices: noSlices}, ex)
	if _, err := a.Preflight(context.Background()); !errors.Is(err, ErrFatal) {
		t.Fatalf("rootless docker + sidecar 는 R16 오류여야 한다: %v", err)
	}
	// none 모드에서는 허용된다(§9.2 는 sidecar 전제다).
	a2 := newAgent(t, Spec{Name: "m1", Local: true, Runtime: domain.RuntimeDocker, Mode: domain.ModeNone}, ex)
	if _, err := a2.Preflight(context.Background()); err != nil {
		t.Fatalf("none 모드는 rootless docker 를 허용한다: %v", err)
	}
}

// TestPreflight_R16_CgroupMismatch: sidecar 전제는 cgroup v2 + systemd 드라이버다. [R16, §9.2]
func TestPreflight_R16_CgroupMismatch(t *testing.T) {
	for _, tc := range []struct{ driver, version string }{
		{"cgroupfs", "2"},
		{"systemd", "1"},
	} {
		ex := &fakeExec{h: uid("0", func(_ bool, argv []string) (executor.Result, error) {
			if isCmd(argv, "docker", "info") {
				return ok(dockerInfoJSON(tc.driver, tc.version))
			}
			return executor.Result{}, nil
		})}
		called := 0
		a := newAgent(t, Spec{Name: "m1", Local: true, Runtime: domain.RuntimeDocker, Mode: domain.ModeSidecar,
			NewSlices: func(executor.Executor, bool) SliceChecker { return fakeSlices{called: &called} }}, ex)
		_, err := a.Preflight(context.Background())
		if !errors.Is(err, ErrFatal) {
			t.Fatalf("%s/%s: ErrFatal 이어야 한다: %v", tc.driver, tc.version, err)
		}
		if called != 0 {
			t.Fatalf("cgroup 위반인데 slice 확인까지 갔다")
		}
	}
}

// TestPreflight_S10_2_SlicesSudoByIdU: 규칙 1·4 — systemctl 은 root 가 아니면 `sudo -n`,
// root 면 붙이지 않는다. [§10.2 규칙 1, 규칙 4]
func TestPreflight_S10_2_SlicesSudoByIdU(t *testing.T) {
	for _, tc := range []struct {
		uidOut   string
		wantSudo bool
	}{
		{"0", false},
		{"1000", true},
	} {
		ex := &fakeExec{h: uid(tc.uidOut, func(_ bool, argv []string) (executor.Result, error) {
			if isCmd(argv, "docker", "info") {
				return ok(dockerInfoJSON("systemd", "2"))
			}
			return executor.Result{}, nil
		})}
		gotSudo, called := false, 0
		a := newAgent(t, Spec{Name: "m1", Local: true, Runtime: domain.RuntimeDocker, Mode: domain.ModeSidecar,
			NewSlices: func(_ executor.Executor, sudo bool) SliceChecker {
				gotSudo = sudo
				return fakeSlices{called: &called}
			}}, ex)
		if _, err := a.Preflight(context.Background()); err != nil {
			t.Fatalf("id -u=%s: %v", tc.uidOut, err)
		}
		if called != 1 || gotSudo != tc.wantSudo {
			t.Fatalf("id -u=%s: slice sudo=%v called=%d, want sudo=%v", tc.uidOut, gotSudo, called, tc.wantSudo)
		}
	}
}

// TestPreflight_R16_SlicesCheckFailure: systemctl 을 못 쓰면 R16 오류다. [R16, §10.2 sidecar 행]
func TestPreflight_R16_SlicesCheckFailure(t *testing.T) {
	ex := &fakeExec{h: uid("1000", func(_ bool, argv []string) (executor.Result, error) {
		if isCmd(argv, "docker", "info") {
			return ok(dockerInfoJSON("systemd", "2"))
		}
		return executor.Result{}, nil
	})}
	called := 0
	a := newAgent(t, Spec{Name: "m1", Local: true, Runtime: domain.RuntimeDocker, Mode: domain.ModeSidecar,
		Images: []string{"img"},
		NewSlices: func(executor.Executor, bool) SliceChecker {
			return fakeSlices{called: &called, err: errors.New("sudo: a password is required")}
		}}, ex)
	_, err := a.Preflight(context.Background())
	if !errors.Is(err, ErrFatal) {
		t.Fatalf("ErrFatal 이어야 한다: %v", err)
	}
	for _, c := range ex.calls() {
		if strings.Contains(c, " pull ") {
			t.Fatalf("R16 오류인데 pre-pull 까지 갔다: %v", ex.calls())
		}
	}
}

// TestPreflight_R21_VerifyFailureIsFatal: Controller 가 넘긴 예산 판정(R21) 위반은 설정·환경 모순이다. [R21, §7.1-3]
func TestPreflight_R21_VerifyFailureIsFatal(t *testing.T) {
	ex := &fakeExec{h: uid("0", func(_ bool, argv []string) (executor.Result, error) {
		if isCmd(argv, "docker", "info") {
			return ok(dockerInfoJSON("systemd", "2"))
		}
		return executor.Result{}, nil
	})}
	a := newAgent(t, Spec{Name: "m1", Local: true, Runtime: domain.RuntimeDocker, Mode: domain.ModeNone,
		Images: []string{"img"},
		Verify: func(runtime.Info) error { return errors.New("R21 resources < unit") }}, ex)
	_, err := a.Preflight(context.Background())
	if !errors.Is(err, ErrFatal) {
		t.Fatalf("ErrFatal 이어야 한다: %v", err)
	}
	if !strings.Contains(err.Error(), "R21") {
		t.Fatalf("원인이 남지 않았다: %v", err)
	}
}

// TestPreflight_S10_1_IdUFailureIsTransient: 접속·명령 실패는 도달 불가(unhealthy)이지 모순이 아니다. [§7.1-3]
func TestPreflight_S10_1_IdUFailureIsTransient(t *testing.T) {
	ex := &fakeExec{h: func(bool, []string) (executor.Result, error) {
		return executor.Result{}, errors.New("ssh: connection lost")
	}}
	a := newAgent(t, Spec{Name: "m1", Local: true, Runtime: domain.RuntimeDocker, Mode: domain.ModeNone}, ex)
	_, err := a.Preflight(context.Background())
	if err == nil || errors.Is(err, ErrFatal) {
		t.Fatalf("도달 불가는 ErrFatal 이 아니다: %v", err)
	}
	if ex.closed != 1 {
		t.Fatalf("실패한 접속을 닫지 않았다: closed=%d", ex.closed)
	}
}

// TestNew_S10_1_SSHRequired: SSH 머신인데 접속 정보가 없으면 조립 자체가 실패한다. [§10.1]
func TestNew_S10_1_SSHRequired(t *testing.T) {
	if _, err := New(Spec{Name: "m1", Runtime: domain.RuntimeDocker}, nil); err == nil {
		t.Fatal("SSH 설정 없는 원격 머신이 통과했다")
	}
	if _, err := New(Spec{Name: "m1", Local: true, Runtime: domain.RuntimeKind("containerd")}, nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("알 수 없는 runtime: %v", err)
	}
}

// TestPreflight_S10_2_PodmanPathStickyAcrossReconnects: 한번 고정된 podman 경로는 재접속에서 다시
// 협상하지 않는다. rootless 경로가 일시적으로 실패해도 rootful 저장소로 갈아타지 않고 unhealthy 로
// 둔다 — 저장소가 바뀌면 이미 도는 unit 이 사라진 것처럼 보이고 재동기화가 그 부품을 지운다. [§10.2 규칙 3, §8.3]
func TestPreflight_S10_2_PodmanPathStickyAcrossReconnects(t *testing.T) {
	rootlessOK := true
	ex := &fakeExec{h: uid("1000", func(sudo bool, argv []string) (executor.Result, error) {
		if isCmd(argv, "podman", "info") {
			if sudo {
				return ok(podmanInfoJSON(false, "systemd", "v2")) // rootful 은 항상 성공
			}
			if !rootlessOK {
				return fail("Cannot connect to Podman socket")
			}
			return ok(podmanInfoJSON(true, "systemd", "v2"))
		}
		return executor.Result{}, nil
	})}
	a := newAgent(t, Spec{Name: "m1", Local: true, Runtime: domain.RuntimePodman, Mode: domain.ModeNone}, ex)
	if _, err := a.Preflight(context.Background()); err != nil {
		t.Fatalf("첫 preflight: %v", err)
	}
	if a.podmanSudo == nil || *a.podmanSudo {
		t.Fatalf("rootless 경로가 고정되지 않았다: %v", a.podmanSudo)
	}

	rootlessOK = false // 재접속 시 고정된 경로가 일시적으로 실패
	_, err := a.Preflight(context.Background())
	if err == nil {
		t.Fatal("고정된 경로가 실패했는데 성공으로 처리했다")
	}
	if errors.Is(err, ErrFatal) {
		t.Fatalf("도달 불가는 unhealthy 다: %v", err)
	}
	for _, c := range ex.calls() {
		if strings.HasPrefix(c, "sudo -n podman info") {
			t.Fatalf("재접속에서 다른 경로로 갈아탔다: %v", ex.calls())
		}
	}
}

// TestPreflight_S8_3_CommandTimeouts: 확인 명령(`id -u`, `info`)과 관측(`ps`, `volume ls`)에는 probe
// 상한(30s), pre-pull 에는 pre-pull 상한(5분)이 걸린다. preflight 는 그 머신의 첫 통지보다 앞이라
// 상한이 없으면 응답 없는 명령 하나가 모든 scale set 의 세션 시작을 막는다. [§7.1-4, §8.3 상수 표]
func TestPreflight_S8_3_CommandTimeouts(t *testing.T) {
	ex := &fakeExec{h: uid("0", func(_ bool, argv []string) (executor.Result, error) {
		if isCmd(argv, "docker", "info") {
			return ok(dockerInfoJSON("systemd", "2"))
		}
		return executor.Result{}, nil
	})}
	a := newAgent(t, Spec{Name: "m1", Local: true, Runtime: domain.RuntimeDocker, Mode: domain.ModeNone,
		Images: []string{"img"}}, ex)
	if _, err := a.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if _, err := a.Observe(context.Background()); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	for _, tc := range []struct {
		prefix string
		want   time.Duration
	}{
		{"id -u", probeTimeout},
		{"docker info", probeTimeout},
		{"docker pull ", pullTimeout},
		{"docker ps ", probeTimeout},
		{"docker volume ls", probeTimeout},
	} {
		got, found := ex.budgetFor(tc.prefix)
		if !found {
			t.Fatalf("%q 에 상한이 없다", tc.prefix)
		}
		// 명령 실행까지의 경과분만큼 줄어드니 want 이하 · want-5s 초과여야 한다.
		if got > tc.want || got < tc.want-5*time.Second {
			t.Fatalf("%q 상한 %v, want ≈%v", tc.prefix, got, tc.want)
		}
	}
}

// TestRun_S7_1_4_PrePullOrderByRound: pre-pull 의 자리는 "최초 접속인가"로 갈린다. 최초 접속 회차는
// 동기화 앞에서 pull 하고(콜드 머신은 이미지가 실제로 없다), 이미 한 번 성공했던 머신의 재접속
// 회차는 events·info·관측·Resynced 뒤로 미룬다 — 레지스트리 장애가 재동기화와 die 수신까지 막으면
// 안 된다. 명령 로그의 순서가 그 계약이다. [§7.1 재접속 회차, §7.1-4]
func TestRun_S7_1_4_PrePullOrderByRound(t *testing.T) {
	// 1회차 스트림은 즉시 EOF(회차 실패 → 재접속), 2회차부터는 열린 채로 둔다.
	ex := &fakeExec{holdAfter: 1, h: uid("0", func(_ bool, argv []string) (executor.Result, error) {
		if isCmd(argv, "docker", "info") {
			return ok(dockerInfoJSON("systemd", "2"))
		}
		return executor.Result{}, nil
	})}
	// SSH 머신이라 회차마다 접속을 버리고 preflight 부터 다시 돈다(§10.1) — 회차 경계가 `id -u` 다.
	a, err := New(Spec{Name: "m1", Runtime: domain.RuntimeDocker, Mode: domain.ModeNone,
		Images:      []string{"img:1"},
		NewExecutor: func() (executor.Executor, error) { return ex, nil }}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sink := &recSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	a.Run(ctx, sink)

	// 회차 경계는 `id -u`(preflight 시작)다.
	var rounds [][]string
	for _, c := range ex.calls() {
		if strings.HasPrefix(c, "id -u") {
			rounds = append(rounds, nil)
		}
		if len(rounds) > 0 {
			rounds[len(rounds)-1] = append(rounds[len(rounds)-1], c)
		}
	}
	if len(rounds) < 2 {
		t.Fatalf("회차 %d개, want ≥2: %v", len(rounds), ex.calls())
	}
	idx := func(round []string, prefix string) int {
		for i, c := range round {
			if strings.HasPrefix(c, prefix) {
				return i
			}
		}
		return -1
	}
	if p, e := idx(rounds[0], "docker pull"), idx(rounds[0], "docker events"); p < 0 || e < 0 || p > e {
		t.Fatalf("최초 접속 회차: pull=%d events=%d, want pull 이 앞: %v", p, e, rounds[0])
	}
	r := rounds[1]
	p, v := idx(r, "docker pull"), idx(r, "docker volume ls")
	if p < 0 || v < 0 || p < v {
		t.Fatalf("재접속 회차: pull=%d volumeLs=%d, want pull 이 동기화 뒤: %v", p, v, r)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.resynced == 0 {
		t.Fatal("재접속 회차가 Resynced 까지 가지 못했다")
	}
}
