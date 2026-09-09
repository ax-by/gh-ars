package runtime

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"gh-ars/internal/domain"
	"gh-ars/internal/executor"
)

func newDockerFake(t *testing.T) (*fakeExec, Runtime) {
	t.Helper()
	f := &fakeExec{t: t}
	return f, NewDocker(f, false, testLogger())
}

// [DESIGN §4.2] docker 구현이 Kind 와 받은 sudo 값을 명령에 그대로 전파하는지 본다. "docker 는
// sudo 를 쓰지 않는다"(§10.2 규칙 2)를 강제하는 곳은 이 생성자가 아니라 preflight 이며, 그
// 판정은 machine 패키지가 검증한다(TestPreflight_S10_2_DockerNoSudo).
func TestDocker_S5_KindAndSudoPropagation(t *testing.T) {
	f, rt := newDockerFake(t)
	if rt.Kind() != domain.RuntimeDocker {
		t.Fatalf("Kind = %v", rt.Kind())
	}
	if err := rt.Start(context.Background(), "gh-ars-x-runner"); err != nil {
		t.Fatal(err)
	}
	if c := f.last(); c.Sudo {
		t.Fatalf("docker 명령에 Sudo 가 켜졌다: %v", c.Argv)
	}
}

// [§8.3, DESIGN §4.2] List 는 항상 `ps -a` 이고 라벨 필터를 그대로 넘긴다. 출력은 탭 구분
// 한 줄에 컨테이너 하나이고 라벨은 gh-ars.* 키를 `{{.Label "key"}}` 로 하나씩 뽑는다.
// `{{json .}}` 의 Labels 문자열은 이미지가 물려준 라벨 값의 콤마를 이스케이프하지 않아
// ",gh-ars.mode=none" 조각이 실제 라벨을 덮어쓸 수 있기 때문이다(Phase 5 리뷰 지적).
func TestDocker_S8_3_ListArgvAndParse(t *testing.T) {
	f, rt := newDockerFake(t)
	f.results = []executor.Result{{Stdout: []byte(
		"gh-ars-01HXYZ-runner\trunning\t2026-09-07 16:03:42 +0900 KST\t01HXYZ\trunner\tss\tnone\tm1\n" +
			"gh-ars-01HABC-sidecar\texited\t2026-09-07 15:00:00 +0000 UTC\t\t\t\t\t\n",
	)}}
	got, err := rt.List(context.Background(), "gh-ars.unit")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantFormat := `{{.Names}}\t{{.State}}\t{{.CreatedAt}}\t{{.Label "gh-ars.unit"}}\t{{.Label "gh-ars.role"}}\t{{.Label "gh-ars.scaleSet"}}\t{{.Label "gh-ars.mode"}}\t{{.Label "gh-ars.machine"}}`
	wantArgv := []string{"docker", "ps", "-a", "--filter", "label=gh-ars.unit", "--format", wantFormat}
	if c := f.last(); !reflect.DeepEqual(c.Argv, wantArgv) {
		t.Fatalf("argv = %q, want %q", c.Argv, wantArgv)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
	c0 := got[0]
	if c0.Name != "gh-ars-01HXYZ-runner" || c0.State != "running" {
		t.Errorf("c0 = %+v", c0)
	}
	wantLabels := map[string]string{
		"gh-ars.machine": "m1", "gh-ars.mode": "none", "gh-ars.role": "runner",
		"gh-ars.scaleSet": "ss", "gh-ars.unit": "01HXYZ",
	}
	if !reflect.DeepEqual(c0.Labels, wantLabels) {
		t.Errorf("labels = %v, want %v", c0.Labels, wantLabels)
	}
	wantCreated := time.Date(2026, 9, 7, 7, 3, 42, 0, time.UTC)
	if !c0.Created.Equal(wantCreated) {
		t.Errorf("Created = %v, want %v", c0.Created, wantCreated)
	}
	if got[1].Name != "gh-ars-01HABC-sidecar" || got[1].State != "exited" || len(got[1].Labels) != 0 {
		t.Errorf("c1 = %+v", got[1])
	}
	// unit 한정 필터도 그대로 전달된다. [§4.2]
	if _, err := rt.List(context.Background(), "gh-ars.unit=01HXYZ"); err != nil {
		t.Fatal(err)
	}
	if c := f.last(); c.Argv[4] != "label=gh-ars.unit=01HXYZ" {
		t.Errorf("unit 필터 argv = %v", c.Argv)
	}
}

// [§8.3] ps 실패(데몬 다운)는 빈 목록이 아니라 오류다. 빈 목록으로 오해하면 전체 동기화가
// 모든 unit 을 잃은 것으로 판정한다.
func TestDocker_S8_3_ListFailure(t *testing.T) {
	f, rt := newDockerFake(t)
	f.results = []executor.Result{{ExitCode: 1, Stderr: []byte("Cannot connect to the Docker daemon")}}
	_, err := rt.List(context.Background(), "gh-ars.unit")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.ExitCode != 1 || !strings.Contains(err.Error(), "docker ps") || !strings.Contains(err.Error(), "Cannot connect") {
		t.Fatalf("err = %v, want ExitError(argv+stderr)", err)
	}
	f.results = []executor.Result{{Stdout: []byte("gh-ars-x-runner\trunning\n")}}
	if _, err := rt.List(context.Background(), "gh-ars.unit"); err == nil {
		t.Fatal("필드 수가 모자란 줄이 오류가 아니다")
	}
	f.results = []executor.Result{{Stdout: []byte("gh-ars-x-runner\trunning\tnot a date\t\t\t\t\t\n")}}
	if _, err := rt.List(context.Background(), "gh-ars.unit"); err == nil {
		t.Fatal("CreatedAt 파싱 실패가 오류가 아니다")
	}
}

// [§7.2-4, §9.3 표] none 모드 create: 이름, 라벨(정렬), --cpus/--memory, entrypoint 래퍼,
// 이미지 뒤에 Cmd. JIT 값은 argv 어디에도 없다.
func TestDocker_S7_2_4_CreateNone(t *testing.T) {
	f, rt := newDockerFake(t)
	u := domain.Unit{ID: "01HXYZ", ScaleSet: "ss", Machine: "m1", Mode: domain.ModeNone}
	ep, cmd, _ := Wrapper(domain.ModeNone)
	spec := CreateSpec{
		Name:        domain.ContainerName(u.ID, domain.RoleRunner),
		Image:       "ghcr.io/actions/actions-runner:2.337.0",
		Labels:      domain.Labels(u, domain.RoleRunner),
		Entrypoint:  ep,
		Cmd:         cmd,
		CPUs:        0.5,
		MemoryBytes: 4294967296,
	}
	if err := rt.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := []string{"docker", "create",
		"--name", "gh-ars-01HXYZ-runner",
		"--label", "gh-ars.machine=m1",
		"--label", "gh-ars.mode=none",
		"--label", "gh-ars.role=runner",
		"--label", "gh-ars.scaleSet=ss",
		"--label", "gh-ars.unit=01HXYZ",
		"--cpus=0.5", "--memory=4294967296",
		"--entrypoint", "/bin/bash",
		"ghcr.io/actions/actions-runner:2.337.0",
		"-c", wrapperNone,
	}
	if c := f.last(); !reflect.DeepEqual(c.Argv, want) {
		t.Fatalf("argv =\n%q\nwant\n%q", c.Argv, want)
	}
	// 예산 0 이면 플래그를 내지 않는다(입양·sidecar 모드). [§9.3]
	spec.CPUs, spec.MemoryBytes = 0, 0
	if err := rt.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	for _, a := range f.last().Argv {
		if strings.HasPrefix(a, "--cpus") || strings.HasPrefix(a, "--memory") {
			t.Fatalf("예산 0 인데 %s 가 나왔다", a)
		}
	}
}

// [§9.1, DESIGN §4.2] CreateSpec 의 나머지 필드(--cgroup-parent, --privileged, 볼륨, env)도
// argv 로 나간다. sidecar 값 자체의 검증은 Phase 12 (runtime/sidecar) 몫이다.
func TestDocker_S9_1_CreateExtraFlags(t *testing.T) {
	f, rt := newDockerFake(t)
	spec := CreateSpec{
		Name:       "gh-ars-01HXYZ-sidecar",
		Image:      "docker:29.7.2-dind",
		Entrypoint: []string{"dockerd"},
		Cmd:        []string{"--host=unix:///var/run/docker.sock"},
		Env:        map[string]string{"DOCKER_TLS_CERTDIR": "", "B": "2", "A": "1"},
		Volumes: []Mount{
			{Volume: "gh-ars-01HXYZ-sock", ContainerPath: "/var/run"},
			{Volume: "gh-ars-01HXYZ-work", ContainerPath: "/home/runner/_work"},
		},
		CgroupParent: "gh-ars-01HXYZ.slice",
		Privileged:   true,
	}
	if err := rt.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := []string{"docker", "create",
		"--name", "gh-ars-01HXYZ-sidecar",
		"--cgroup-parent=gh-ars-01HXYZ.slice",
		"--privileged",
		"-v", "gh-ars-01HXYZ-sock:/var/run",
		"-v", "gh-ars-01HXYZ-work:/home/runner/_work",
		"-e", "A=1", "-e", "B=2", "-e", "DOCKER_TLS_CERTDIR=",
		"--entrypoint", "dockerd",
		"docker:29.7.2-dind",
		"--host=unix:///var/run/docker.sock",
	}
	if c := f.last(); !reflect.DeepEqual(c.Argv, want) {
		t.Fatalf("argv =\n%q\nwant\n%q", c.Argv, want)
	}
}

// [§7.2-4] JIT 값은 컨테이너 env 에 절대 넣지 않는다. 래퍼가 파일에서 읽어 export 하는
// 이름이 Env 에 오면 호출자 버그이므로 실행 전에 거부한다. Entrypoint 는 원소 하나만 받는다.
func TestDocker_S7_2_4_CreateRejectsBadSpec(t *testing.T) {
	f, rt := newDockerFake(t)
	base := CreateSpec{Name: "n", Image: "img", Entrypoint: []string{"/bin/bash"}}
	bad := base
	bad.Env = map[string]string{"ACTIONS_RUNNER_INPUT_JITCONFIG": "secret"}
	if err := rt.Create(context.Background(), bad); err == nil {
		t.Fatal("JIT env 가 거부되지 않았다")
	}
	bad = base
	bad.Entrypoint = []string{"/bin/bash", "-c"}
	if err := rt.Create(context.Background(), bad); err == nil {
		t.Fatal("다중 entrypoint 가 거부되지 않았다")
	}
	bad = base
	bad.Name = ""
	if err := rt.Create(context.Background(), bad); err == nil {
		t.Fatal("빈 이름이 거부되지 않았다")
	}
	if len(f.cmds) != 0 {
		t.Fatalf("거부된 spec 으로 명령이 실행됐다: %v", f.cmds)
	}
}

// [§7.2-4] cp - <ctr>:/home/runner 에 tar 스트림이 stdin 으로 그대로 간다. argv 에는 값이 없다.
func TestDocker_S7_2_4_CopyIn(t *testing.T) {
	f, rt := newDockerFake(t)
	tr, err := JITTar("secret-jit")
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.CopyIn(context.Background(), "gh-ars-01HXYZ-runner", tr, RunnerHome); err != nil {
		t.Fatalf("CopyIn: %v", err)
	}
	c := f.last()
	want := []string{"docker", "cp", "-", "gh-ars-01HXYZ-runner:/home/runner"}
	if !reflect.DeepEqual(c.Argv, want) {
		t.Fatalf("argv = %v, want %v", c.Argv, want)
	}
	if !strings.Contains(f.stdins[0], "secret-jit\n") || !strings.Contains(f.stdins[0], ".jitconfig") {
		t.Fatalf("stdin 에 tar 스트림이 없다")
	}
	for _, a := range c.Argv {
		if strings.Contains(a, "secret-jit") {
			t.Fatalf("argv 에 JIT 값이 있다: %v", c.Argv)
		}
	}
}

// [§7.1-4, §7.2-4] 단순 명령들의 argv. 비0 종료는 argv·stderr 를 담은 오류다.
func TestDocker_S7_SimpleArgv(t *testing.T) {
	f, rt := newDockerFake(t)
	ctx := context.Background()
	steps := []struct {
		name string
		call func() error
		want []string
	}{
		{"pull", func() error { return rt.Pull(ctx, "img:1") }, []string{"docker", "pull", "img:1"}},
		{"start", func() error { return rt.Start(ctx, "c") }, []string{"docker", "start", "c"}},
		{"rm", func() error { return rt.Remove(ctx, "c", false) }, []string{"docker", "rm", "c"}},
		{"rm -f", func() error { return rt.Remove(ctx, "c", true) }, []string{"docker", "rm", "-f", "c"}},
		{"volume create", func() error {
			return rt.VolumeCreate(ctx, "gh-ars-01HXYZ-work", map[string]string{"gh-ars.unit": "01HXYZ", "gh-ars.mode": "sidecar"})
		}, []string{"docker", "volume", "create", "--label", "gh-ars.mode=sidecar", "--label", "gh-ars.unit=01HXYZ", "gh-ars-01HXYZ-work"}},
		{"volume rm", func() error { return rt.VolumeRemove(ctx, "v") }, []string{"docker", "volume", "rm", "v"}},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			if err := s.call(); err != nil {
				t.Fatalf("%v", err)
			}
			if c := f.last(); !reflect.DeepEqual(c.Argv, s.want) {
				t.Fatalf("argv = %v, want %v", c.Argv, s.want)
			}
			f.results = []executor.Result{{ExitCode: 125, Stderr: []byte("some failure")}}
			err := s.call()
			var ee *ExitError
			if !errors.As(err, &ee) || ee.ExitCode != 125 || !strings.Contains(err.Error(), "some failure") {
				t.Fatalf("비0 종료 err = %v", err)
			}
			f.results = nil
			f.runErr = errBoom
			if err := s.call(); !errors.Is(err, errBoom) {
				t.Fatalf("실행 실패가 전파되지 않았다: %v", err)
			}
			f.runErr = nil
		})
	}
}

// [§8.3] 정리 단계는 멱등이다. 이미 없는 컨테이너·볼륨의 rm 은 성공이다.
func TestDocker_S8_3_RemoveIdempotent(t *testing.T) {
	f, rt := newDockerFake(t)
	ctx := context.Background()
	f.results = []executor.Result{{ExitCode: 1, Stderr: []byte("Error response from daemon: No such container: gh-ars-x-runner\n")}}
	if err := rt.Remove(ctx, "gh-ars-x-runner", true); err != nil {
		t.Fatalf("없는 컨테이너 rm 이 오류: %v", err)
	}
	f.results = []executor.Result{{ExitCode: 1, Stderr: []byte("Error response from daemon: get gh-ars-x-work: no such volume\n")}}
	if err := rt.VolumeRemove(ctx, "gh-ars-x-work"); err != nil {
		t.Fatalf("없는 볼륨 rm 이 오류: %v", err)
	}
	// 다른 실패(사용 중 볼륨)는 오류다.
	f.results = []executor.Result{{ExitCode: 1, Stderr: []byte("Error response from daemon: remove gh-ars-x-work: volume is in use\n")}}
	if err := rt.VolumeRemove(ctx, "gh-ars-x-work"); err == nil {
		t.Fatal("사용 중 볼륨 rm 실패가 성공으로 나왔다")
	}
}

// [§4.2] volume ls 는 라벨 필터로 이름만 받는다.
func TestDocker_S4_2_VolumeList(t *testing.T) {
	f, rt := newDockerFake(t)
	f.results = []executor.Result{{Stdout: []byte("gh-ars-01HXYZ-work\ngh-ars-01HXYZ-sock\n\n")}}
	got, err := rt.VolumeList(context.Background(), "gh-ars.unit")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"docker", "volume", "ls", "--filter", "label=gh-ars.unit", "--format", "{{.Name}}"}
	if c := f.last(); !reflect.DeepEqual(c.Argv, want) {
		t.Fatalf("argv = %v, want %v", c.Argv, want)
	}
	if !reflect.DeepEqual(got, []string{"gh-ars-01HXYZ-work", "gh-ars-01HXYZ-sock"}) {
		t.Fatalf("got = %v", got)
	}
}

// [§7.1-3, §9.2] Info 는 `info --format '{{json .}}'` 에서 NCPU/MemTotal/CgroupDriver/
// CgroupVersion 을 읽는다. ServerErrors 가 있으면 데몬에 못 닿은 것이므로 오류다.
func TestDocker_S9_2_InfoParse(t *testing.T) {
	f, rt := newDockerFake(t)
	f.results = []executor.Result{{Stdout: []byte(`{"ID":"x","NCPU":10,"MemTotal":8321515520,"CgroupDriver":"systemd","CgroupVersion":"2","SecurityOptions":["name=seccomp,profile=builtin"]}` + "\n")}}
	got, err := rt.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	want := []string{"docker", "info", "--format", "{{json .}}"}
	if c := f.last(); !reflect.DeepEqual(c.Argv, want) {
		t.Fatalf("argv = %v, want %v", c.Argv, want)
	}
	wantInfo := Info{CPUs: 10, MemoryBytes: 8321515520, CgroupDriver: "systemd", CgroupVersion: "2"}
	if got != wantInfo {
		t.Fatalf("Info = %+v, want %+v", got, wantInfo)
	}
	f.results = []executor.Result{{Stdout: []byte(`{"NCPU":0,"MemTotal":0,"ServerErrors":["Cannot connect to the Docker daemon"]}`)}}
	if _, err := rt.Info(context.Background()); err == nil || !strings.Contains(err.Error(), "Cannot connect") {
		t.Fatalf("ServerErrors 가 오류가 아니다: %v", err)
	}
	f.results = []executor.Result{{ExitCode: 1, Stderr: []byte("permission denied")}}
	if _, err := rt.Info(context.Background()); err == nil {
		t.Fatal("비0 종료가 오류가 아니다")
	}
	// 예산이 없는 info(스키마 어긋남)는 조용한 0 이 아니라 오류다: 0 은 R21 에서 physicalMax 0 →
	// 영구 Failed 로 번지는데, 원인은 스키마 불일치이므로 재시도 가능한 unhealthy 여야 한다. [§7.1-3, R21]
	f.results = []executor.Result{{Stdout: []byte(`{"CgroupDriver":"systemd","CgroupVersion":"2"}`)}}
	if _, err := rt.Info(context.Background()); err == nil {
		t.Fatal("NCPU·MemTotal 이 없는 info 가 성공으로 처리됐다")
	}
}

// [§7.1-8, §4.2] events: 라벨·type 필터를 건 `{{json .}}` 스트림을 {Name, Action, ExitCode, At} 로
// 정규화한다. 아래 줄들은 실제 docker 29 출력이다. 스트림 종료 시 error 채널로 통지하고
// 두 채널을 닫는다.
func TestDocker_S7_1_8_EventsNormalize(t *testing.T) {
	f, rt := newDockerFake(t)
	f.stream = strings.Join([]string{
		`{"Type":"container","Action":"create","Actor":{"ID":"f041","Attributes":{"gh-ars.role":"runner","gh-ars.unit":"X","image":"img","name":"gh-ars-X-runner"}},"scope":"local","time":1788770856,"timeNano":1788770856055853595}`,
		`not json at all`,
		`{"Type":"container","Action":"start","Actor":{"ID":"f041","Attributes":{"name":"gh-ars-X-runner"}},"scope":"local","time":1788770856,"timeNano":1788770856149386678}`,
		`{"Type":"container","Action":"die","Actor":{"ID":"f041","Attributes":{"execDuration":"0","exitCode":"3","name":"gh-ars-X-runner"}},"scope":"local","time":1788770856,"timeNano":1788770856212629470}`,
		`{"Type":"network","Action":"connect","Actor":{"ID":"n1","Attributes":{"container":"f041","name":"bridge"}},"scope":"local","time":1788770856,"timeNano":1788770856300000000}`,
		"",
	}, "\n")
	evCh, errCh := rt.Events(context.Background(), "gh-ars.unit")
	wantArgv := []string{"docker", "events", "--filter", "label=gh-ars.unit", "--filter", "type=container", "--format", "{{json .}}"}
	if c := f.last(); !reflect.DeepEqual(c.Argv, wantArgv) {
		t.Fatalf("argv = %v, want %v", c.Argv, wantArgv)
	}
	var got []Event
	for ev := range evCh {
		got = append(got, ev)
	}
	want := []Event{
		{Name: "gh-ars-X-runner", Action: "create", At: time.Unix(0, 1788770856055853595)},
		{Name: "gh-ars-X-runner", Action: "start", At: time.Unix(0, 1788770856149386678)},
		{Name: "gh-ars-X-runner", Action: "die", ExitCode: 3, At: time.Unix(0, 1788770856212629470)},
	}
	if len(got) != len(want) {
		t.Fatalf("events = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Action != want[i].Action || got[i].ExitCode != want[i].ExitCode || !got[i].At.Equal(want[i].At) {
			t.Errorf("event[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	err, ok := <-errCh
	if !ok || err == nil {
		t.Fatalf("스트림 종료 통지가 없다: ok=%v err=%v", ok, err)
	}
	if _, ok := <-errCh; ok {
		t.Fatal("error 채널이 닫히지 않았다")
	}
	if f.closed != 1 {
		t.Fatalf("스트림 Close 횟수 = %d, want 1", f.closed)
	}
}

// [§7.1-8] Stream 을 열지 못하면(실행 실패) error 채널로 즉시 통지한다.
func TestDocker_S7_1_8_EventsOpenFailure(t *testing.T) {
	f, rt := newDockerFake(t)
	f.strmErr = errBoom
	evCh, errCh := rt.Events(context.Background(), "gh-ars.unit")
	if err := <-errCh; !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := <-evCh; ok {
		t.Fatal("event 채널이 닫히지 않았다")
	}
}

// [§7.1-8] 소비자가 없는 상태에서 ctx 가 취소되면 goroutine 이 남지 않는다.
func TestDocker_S7_1_8_EventsCtxCancel(t *testing.T) {
	f, rt := newDockerFake(t)
	f.stream = `{"Type":"container","Action":"start","Actor":{"Attributes":{"name":"gh-ars-X-runner"}},"timeNano":1}` + "\n"
	ctx, cancel := context.WithCancel(context.Background())
	_, errCh := rt.Events(ctx, "gh-ars.unit")
	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("취소 통지가 nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("취소 후에도 error 채널 통지가 없다")
	}
}

// TestDocker_S4_2_UnparseableLineSignal: 읽지 못한 줄은 첫 줄 즉시 경고 + 종료 오류의 집계로
// 알린다(podman 과 같은 계약. TESTPLAN 은 두 flavor 모두에 이 항목을 건다). [DESIGN §4.2, §7.2-5]
func TestDocker_S4_2_UnparseableLineSignal(t *testing.T) {
	var logs bytes.Buffer
	f := &fakeExec{}
	f.stream = strings.Join([]string{
		`not json at all`,
		`{"Type":"network","Action":"connect","Actor":{"Attributes":{"name":"bridge"}},"timeNano":1}`,
		`{"Type":"container","Action":"die","Actor":{"Attributes":{"name":"gh-ars-X-runner","exitCode":"0"}},"timeNano":2}`,
		"",
	}, "\n")
	rt := NewDocker(f, false, slog.New(slog.NewTextHandler(&logs, nil)))

	evCh, errCh := rt.Events(context.Background(), "gh-ars.unit")
	var got []Event
	for ev := range evCh {
		got = append(got, ev)
	}
	if len(got) != 1 || got[0].Action != ActionDie {
		t.Fatalf("events = %+v, want die 1건", got)
	}
	err := <-errCh
	if err == nil || !strings.Contains(err.Error(), "읽지 못한 줄 2개") {
		t.Fatalf("종료 오류에 집계가 없다: %v", err)
	}
	if n := strings.Count(logs.String(), "읽지 못했다"); n != 1 {
		t.Fatalf("경고 %d회, want 1 (첫 줄만 즉시, 나머지는 집계)", n)
	}
}

// TestPS_S7_1_7_NumericZoneName: zone 이름이 숫자 오프셋인 호스트에서도 `ps` 한 줄을 읽는다.
// Asia/Kathmandu(+0545)·Asia/Tehran(+0330) 같은 TZ 에서 `time.Time.String()` 은 "… +0545 +0545"
// 를 내는데, Go 는 그 zone 이름을 인정하지 않는다(parseSignedOffset 의 23시간 상한). 한 줄 실패가
// List 전체를 오류로 만들면 그 머신은 매 회차 Observe 실패로 영구히 unhealthy 가 된다. [§7.1-7, §8.3]
func TestPS_S7_1_7_NumericZoneName(t *testing.T) {
	for _, tc := range []struct {
		name, value, layout string
		wantOffsetSec       int
	}{
		{"docker +0545", "2026-09-07 16:03:42 +0545 +0545", dockerPSCreatedLayout, 5*3600 + 45*60},
		{"podman +0330", "2026-09-07 16:03:42.123456789 +0330 +0330", podmanPSCreatedLayout, 3*3600 + 30*60},
		{"docker 이름 있는 zone", "2026-09-07 16:03:42 +0900 KST", dockerPSCreatedLayout, 9 * 3600},
	} {
		got, err := parsePSTime(tc.value, tc.layout)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if _, off := got.Zone(); off != tc.wantOffsetSec {
			t.Fatalf("%s: 오프셋 %d초, want %d초", tc.name, off, tc.wantOffsetSec)
		}
		if got.UTC().Format("2006-01-02 15:04:05") == "" {
			t.Fatalf("%s: 시각이 비었다", tc.name)
		}
	}
	if _, err := parsePSTime("not a date", dockerPSCreatedLayout); err == nil {
		t.Fatal("진짜 깨진 값은 오류여야 한다")
	}
}

// TestDocker_S4_2_EventWithoutNameIsUnparseable: 구독이 이미 type=container 로 좁혀져 있으므로
// 이름 없는 줄은 "관심 없는 줄"이 아니라 스키마 어긋남이다. 조용히 버리면 die 가 전부 사라져도
// 아무 신호가 남지 않는다(podman 시각 필드에서 실제로 겪은 실패). [§4.2, §7.2-5]
func TestDocker_S4_2_EventWithoutNameIsUnparseable(t *testing.T) {
	_, ok, err := dockerFlavor{}.parseEvent([]byte(`{"Type":"container","Action":"die","Actor":{"Attributes":{}},"timeNano":1}`))
	if ok || err == nil {
		t.Fatalf("ok=%v err=%v, want 읽지 못한 줄", ok, err)
	}
	// 구독이 type=container 로 좁혀져 있으므로 다른 type 이 오는 것도 이상 신호다.
	_, ok, err = dockerFlavor{}.parseEvent([]byte(`{"Type":"network","Action":"connect","Actor":{"Attributes":{}}}`))
	if ok || err == nil {
		t.Fatalf("ok=%v err=%v, want 읽지 못한 줄", ok, err)
	}
}
