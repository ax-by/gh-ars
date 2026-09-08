package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"gh-ars/internal/domain"
	"gh-ars/internal/executor"
)

func newPodmanFake(t *testing.T, sudo bool) (*fakeExec, Runtime) {
	t.Helper()
	f := &fakeExec{t: t}
	return f, NewPodman(f, sudo)
}

// [DESIGN §4.2, §10.2 규칙 3] podman 은 docker 와 달리 sudo 가 true 일 수 있고(rootful podman
// 을 passwordless sudo 로 쓰는 경로), 그 값이 모든 명령의 Cmd.Sudo 로 그대로 전파된다.
func TestPodman_S10_2_KindAndSudoPropagation(t *testing.T) {
	f, rt := newPodmanFake(t, false)
	if rt.Kind() != domain.RuntimePodman {
		t.Fatalf("Kind = %v", rt.Kind())
	}
	if err := rt.Start(context.Background(), "gh-ars-x-runner"); err != nil {
		t.Fatal(err)
	}
	if c := f.last(); c.Sudo || c.Argv[0] != "podman" {
		t.Fatalf("sudo=false 인데 Cmd = %+v", c)
	}

	f2, rt2 := newPodmanFake(t, true)
	if err := rt2.Start(context.Background(), "gh-ars-x-runner"); err != nil {
		t.Fatal(err)
	}
	if c := f2.last(); !c.Sudo {
		t.Fatalf("sudo=true 인데 Cmd.Sudo 가 전파되지 않았다: %+v", c)
	}
	// [§10.2 규칙 3] 고정된 경로는 podman 명령 전부(create, ps, events, info, volume ...)에 적용된다.
	if _, err := rt2.List(context.Background(), "gh-ars.unit"); err != nil {
		t.Fatal(err)
	}
	if c := f2.last(); !c.Sudo {
		t.Fatalf("List 에 sudo 가 전파되지 않았다: %+v", c)
	}
}

// [§8.3, DESIGN §4.2] podman ps 는 docker 와 같은 템플릿 함수를 쓰지만 CreatedAt 은
// `time.Time.String()` 그대로라 소수점 나노초가 붙는다(docker 는 초 단위로 자른다).
func TestPodman_S8_3_ListArgvAndParse(t *testing.T) {
	f, rt := newPodmanFake(t, false)
	f.results = []executor.Result{{Stdout: []byte(
		"gh-ars-01HXYZ-runner\trunning\t2026-09-07 16:03:42.123456789 +0900 KST\t01HXYZ\trunner\tss\tnone\tm1\n",
	)}}
	got, err := rt.List(context.Background(), "gh-ars.unit")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantFormat := `{{.Names}}\t{{.State}}\t{{.CreatedAt}}\t{{.Label "gh-ars.unit"}}\t{{.Label "gh-ars.role"}}\t{{.Label "gh-ars.scaleSet"}}\t{{.Label "gh-ars.mode"}}\t{{.Label "gh-ars.machine"}}`
	wantArgv := []string{"podman", "ps", "-a", "--filter", "label=gh-ars.unit", "--format", wantFormat}
	if c := f.last(); !reflect.DeepEqual(c.Argv, wantArgv) {
		t.Fatalf("argv = %q, want %q", c.Argv, wantArgv)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(got), got)
	}
	wantCreated := time.Date(2026, 9, 7, 7, 3, 42, 123456789, time.UTC)
	if !got[0].Created.Equal(wantCreated) {
		t.Errorf("Created = %v, want %v", got[0].Created, wantCreated)
	}
}

// [§9.2, §10.2 규칙 3, DESIGN §4.2] Info: docker 와 달리 Host 아래 중첩되고, cgroup 버전은
// "v2" → "2" 로 정규화하며, Rootless 는 Host.Security.Rootless 에서 읽는다.
func TestPodman_S9_2_InfoParse(t *testing.T) {
	f, rt := newPodmanFake(t, false)
	f.results = []executor.Result{{Stdout: []byte(`{"host":{"arch":"arm64","cgroupManager":"systemd","cgroupVersion":"v2","cpus":8,"memTotal":17179869184,"security":{"rootless":true,"selinuxEnabled":false}},"store":{}}` + "\n")}}
	got, err := rt.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	wantArgv := []string{"podman", "info", "--format", "{{json .}}"}
	if c := f.last(); !reflect.DeepEqual(c.Argv, wantArgv) {
		t.Fatalf("argv = %v, want %v", c.Argv, wantArgv)
	}
	want := Info{CPUs: 8, MemoryBytes: 17179869184, Rootless: true, CgroupDriver: "systemd", CgroupVersion: "2"}
	if got != want {
		t.Fatalf("Info = %+v, want %+v", got, want)
	}
	// rootful (Rootless=false) 도 그대로 전달된다.
	f.results = []executor.Result{{Stdout: []byte(`{"host":{"cgroupManager":"systemd","cgroupVersion":"v2","security":{"rootless":false}}}`)}}
	got, err = rt.Info(context.Background())
	if err != nil || got.Rootless {
		t.Fatalf("Info = %+v, err = %v, want Rootless=false", got, err)
	}
}

// [§5, TESTPLAN (runtime/podman)] events: `--format json`(docker 의 `{{json .}}` 템플릿이
// 아니다). Status "died" 를 공통 어휘 "die" 로 정규화하고, ContainerExitCode(최상위 필드,
// docker 처럼 Actor.Attributes 를 거치지 않는다)를 ExitCode 로 옮긴다. Time 은 RFC3339Nano
// 문자열이다. 아래 die 이외 줄들은 podman-events(1) 문서의 실제 예시다.
func TestPodman_S4_2_EventsNormalize(t *testing.T) {
	f, rt := newPodmanFake(t, false)
	f.stream = strings.Join([]string{
		`{"ID":"683b0909d556a9c02fa8cd2b61c3531a965db42158627622d1a67b391964d519","Image":"localhost/myshdemo:latest","Name":"gh-ars-X-runner","Status":"start","Time":"2019-04-27T22:47:00.849932843-04:00","Type":"container"}`,
		`not json at all`,
		`{"ID":"683b0909d556a9c02fa8cd2b61c3531a965db42158627622d1a67b391964d519","ContainerExitCode":3,"Name":"gh-ars-X-runner","Status":"died","Time":"2019-04-27T22:47:05.212629470-04:00","Type":"container"}`,
		`{"ID":"n1","Name":"bridge","Status":"connect","Time":"2019-04-27T22:47:05.3-04:00","Type":"network"}`,
		"",
	}, "\n")
	evCh, errCh := rt.Events(context.Background(), "gh-ars.unit")
	wantArgv := []string{"podman", "events", "--filter", "label=gh-ars.unit", "--filter", "type=container", "--format", "json"}
	if c := f.last(); !reflect.DeepEqual(c.Argv, wantArgv) {
		t.Fatalf("argv = %v, want %v", c.Argv, wantArgv)
	}
	var got []Event
	for ev := range evCh {
		got = append(got, ev)
	}
	wantTime0, _ := time.Parse(time.RFC3339Nano, "2019-04-27T22:47:00.849932843-04:00")
	wantTime1, _ := time.Parse(time.RFC3339Nano, "2019-04-27T22:47:05.212629470-04:00")
	want := []Event{
		{Name: "gh-ars-X-runner", Action: "start", At: wantTime0},
		{Name: "gh-ars-X-runner", Action: "die", ExitCode: 3, At: wantTime1},
	}
	if len(got) != len(want) {
		t.Fatalf("events = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Action != want[i].Action || got[i].ExitCode != want[i].ExitCode || !got[i].At.Equal(want[i].At) {
			t.Errorf("event[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if err, ok := <-errCh; !ok || err == nil {
		t.Fatalf("스트림 종료 통지가 없다: ok=%v err=%v", ok, err)
	}
}

// [§8.3] podman rm / volume rm 도 docker 와 같은 부분 문자열로 "이미 없음"을 판별한다.
func TestPodman_S8_3_RemoveIdempotent(t *testing.T) {
	f, rt := newPodmanFake(t, false)
	ctx := context.Background()
	f.results = []executor.Result{{ExitCode: 1, Stderr: []byte("Error: no container with name or ID \"gh-ars-x-runner\" found: no such container\n")}}
	if err := rt.Remove(ctx, "gh-ars-x-runner", true); err != nil {
		t.Fatalf("없는 컨테이너 rm 이 오류: %v", err)
	}
	f.results = []executor.Result{{ExitCode: 1, Stderr: []byte("Error: no volume with name \"gh-ars-x-work\" found: no such volume\n")}}
	if err := rt.VolumeRemove(ctx, "gh-ars-x-work"); err != nil {
		t.Fatalf("없는 볼륨 rm 이 오류: %v", err)
	}
	f.results = []executor.Result{{ExitCode: 1, Stderr: []byte("Error: volume gh-ars-x-work is being used\n")}}
	if err := rt.VolumeRemove(ctx, "gh-ars-x-work"); err == nil {
		t.Fatal("사용 중 볼륨 rm 실패가 성공으로 나왔다")
	}
	var ee *ExitError
	f.results = []executor.Result{{ExitCode: 1, Stderr: []byte("boom")}}
	if _, err := rt.List(ctx, "gh-ars.unit"); !errors.As(err, &ee) {
		t.Fatalf("List 실패가 ExitError 가 아니다: %v", err)
	}
}
