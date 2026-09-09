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

func newPodmanFake(t *testing.T, sudo bool) (*fakeExec, Runtime) {
	t.Helper()
	f := &fakeExec{t: t}
	return f, NewPodman(f, sudo, testLogger())
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
	f.results = []executor.Result{{Stdout: []byte(`{"host":{"cgroupManager":"systemd","cgroupVersion":"v2","cpus":4,"memTotal":8589934592,"security":{"rootless":false}}}`)}}
	got, err = rt.Info(context.Background())
	if err != nil || got.Rootless {
		t.Fatalf("Info = %+v, err = %v, want Rootless=false", got, err)
	}
	// 예산이 없는 info(스키마 어긋남)는 조용한 0 이 아니라 오류다: 0 은 R21 에서 physicalMax 0 →
	// 영구 Failed 로 번지는데, 원인은 스키마 불일치이므로 재시도 가능한 unhealthy 여야 한다. [§7.1-3, R21]
	f.results = []executor.Result{{Stdout: []byte(`{"host":{"cgroupManager":"systemd","cgroupVersion":"v2","security":{"rootless":false}}}`)}}
	if _, err := rt.Info(context.Background()); err == nil {
		t.Fatal("cpus·memTotal 이 없는 info 가 성공으로 처리됐다")
	}
}

// [§5, TESTPLAN (runtime/podman)] events: `--format json`(docker 의 `{{json .}}` 템플릿이
// 아니다). Status "died" 를 공통 어휘 "die" 로 정규화하고, ContainerExitCode(최상위 필드,
// docker 처럼 Actor.Attributes 를 거치지 않는다)를 ExitCode 로 옮긴다.
//
// 아래 줄들은 **실제 podman 6.1.1 출력을 그대로 캡처한 것**이다(2026-09-09, macOS + podman
// machine). 이전 픽스처는 문서 예시를 보고 지어낸 RFC3339 문자열 `"Time"` 이었는데, 실물은
// 유닉스 정수 `time`/`timeNano` 라 unmarshal 이 실패했고 파서가 **모든 이벤트를 버렸다**
// (die 가 영영 오지 않아 §7.2-5 정리 경로가 죽어 있었다). 정수·문자열 양쪽을 받는다.
func TestPodman_S4_2_EventsNormalize(t *testing.T) {
	f, rt := newPodmanFake(t, false)
	f.stream = strings.Join([]string{
		`{"ContainerExitCode":0,"ID":"52c7276fc13f","Image":"docker.io/library/alpine:3.20","Name":"gh-ars-X-runner","Status":"start","time":1788934693,"timeNano":1788934693113568389,"Type":"container","Attributes":{"gh-ars.unit":"X"}}`,
		`not json at all`,
		`{"ContainerExitCode":3,"ID":"52c7276fc13f","Image":"docker.io/library/alpine:3.20","Name":"gh-ars-X-runner","Status":"died","time":1788934694,"timeNano":1788934694175539027,"Type":"container","Attributes":{"gh-ars.unit":"X"}}`,
		// 구버전·문서 형식(RFC3339 문자열 "Time")도 계속 읽는다.
		`{"ID":"683b0909d556","Name":"gh-ars-Y-runner","Status":"died","Time":"2019-04-27T22:47:05.212629470-04:00","Type":"container"}`,
		`{"ID":"n1","Name":"bridge","Status":"connect","time":1788934695,"Type":"network"}`,
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
	wantTimeOld, _ := time.Parse(time.RFC3339Nano, "2019-04-27T22:47:05.212629470-04:00")
	want := []Event{
		{Name: "gh-ars-X-runner", Action: "start", At: time.Unix(0, 1788934693113568389)},
		{Name: "gh-ars-X-runner", Action: "die", ExitCode: 3, At: time.Unix(0, 1788934694175539027)},
		{Name: "gh-ars-Y-runner", Action: "die", At: wantTimeOld},
	}
	if len(got) != len(want) {
		t.Fatalf("events = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Action != want[i].Action || got[i].ExitCode != want[i].ExitCode || !got[i].At.Equal(want[i].At) {
			t.Errorf("event[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	// 읽지 못한 줄(`not json at all`)은 건너뛰되 종료 오류가 그 사실을 알린다: 스키마가 통째로
	// 어긋나 이벤트가 하나도 안 오는 상태를 "조용한 정상" 과 구분할 수 있어야 한다. [§7.2-5]
	err, ok := <-errCh
	if !ok || err == nil {
		t.Fatalf("스트림 종료 통지가 없다: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(err.Error(), "읽지 못한 줄 2개") { // 깨진 JSON 1 + network 1
		t.Fatalf("파싱 실패가 종료 오류에 실리지 않았다: %v", err)
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

// TestPodman_S4_2_EventWithoutNameIsUnparseable: docker 와 같은 규칙 — type=container 인데 이름이
// 없으면 스키마 어긋남으로 세어 종료 오류에 신호를 남긴다. [§4.2, §7.2-5]
func TestPodman_S4_2_EventWithoutNameIsUnparseable(t *testing.T) {
	_, ok, err := podmanFlavor{}.parseEvent([]byte(`{"Type":"container","Status":"died","time":1788934694}`))
	if ok || err == nil {
		t.Fatalf("ok=%v err=%v, want 읽지 못한 줄", ok, err)
	}
	// 구독이 type=container 로 좁혀져 있으므로 다른 type 이 오는 것도 이상 신호다.
	_, ok, err = podmanFlavor{}.parseEvent([]byte(`{"Type":"network","Status":"connect","Name":"bridge","time":1788934695}`))
	if ok || err == nil {
		t.Fatalf("ok=%v err=%v, want 읽지 못한 줄", ok, err)
	}
}

// TestEvents_S4_2_FirstUnparseableLineWarns: 첫 읽지 못한 줄은 즉시 경고로 남는다. 스키마가
// 통째로 어긋나면 스트림은 열린 채 유지되고 모든 줄이 버려지는데, 종료 오류만으로는 그 신호가
// 영영 나오지 않기 때문이다(재시작 트리거가 오지 않는다). [DESIGN §4.2, §7.1-8]
func TestEvents_S4_2_FirstUnparseableLineWarns(t *testing.T) {
	var logs bytes.Buffer
	f := &fakeExec{}
	f.stream = strings.Join([]string{
		`{"Type":"container","Status":"died","time":1788934694}`, // 이름 없음 → 읽지 못한 줄
		`{"Type":"container","Status":"died","Name":"gh-ars-X-runner","time":1788934695}`,
		"",
	}, "\n")
	rt := NewPodman(f, false, slog.New(slog.NewTextHandler(&logs, nil)))

	evCh, errCh := rt.Events(context.Background(), "gh-ars.unit")
	for range evCh {
	}
	<-errCh
	if !strings.Contains(logs.String(), "읽지 못했다") {
		t.Fatalf("첫 실패 줄에 대한 경고가 없다: %q", logs.String())
	}
	if n := strings.Count(logs.String(), "읽지 못했다"); n != 1 {
		t.Fatalf("경고 %d회, want 1 (이후는 종료 오류의 집계로 갈음)", n)
	}
}
