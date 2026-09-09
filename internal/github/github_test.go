package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

// fakeAPI 는 scaleset.Client 대역이다. 호출 인자를 기록하고 미리 넣어 둔 응답을 돌려준다. [DESIGN §11]
type fakeAPI struct {
	groupArg   string
	group      *scaleset.RunnerGroup
	groupErr   error
	getArgs    []any // groupID, name
	set        *scaleset.RunnerScaleSet
	getErr     error
	created    *scaleset.RunnerScaleSet
	createErr  error
	deletedID  int
	deleteErr  error
	jitSetting *scaleset.RunnerScaleSetJitRunnerSetting
	jitSetID   int
	jit        *scaleset.RunnerScaleSetJitRunnerConfig
	jitErr     error
	byNameArg  string
	runner     *scaleset.RunnerReference
	byNameErr  error
	removedID  int64
	removeErr  error
	sessionArg []any // scaleSetID, owner
	session    *scaleset.MessageSessionClient
	sessionErr error
}

func (f *fakeAPI) GetRunnerGroupByName(_ context.Context, name string) (*scaleset.RunnerGroup, error) {
	f.groupArg = name
	return f.group, f.groupErr
}

func (f *fakeAPI) GetRunnerScaleSet(_ context.Context, groupID int, name string) (*scaleset.RunnerScaleSet, error) {
	f.getArgs = []any{groupID, name}
	return f.set, f.getErr
}

func (f *fakeAPI) CreateRunnerScaleSet(_ context.Context, s *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	f.created = s
	if f.createErr != nil {
		return nil, f.createErr
	}
	out := *s
	out.ID = 77
	return &out, nil
}

func (f *fakeAPI) DeleteRunnerScaleSet(_ context.Context, id int) error {
	f.deletedID = id
	return f.deleteErr
}

func (f *fakeAPI) GenerateJitRunnerConfig(_ context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, id int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	f.jitSetting, f.jitSetID = s, id
	return f.jit, f.jitErr
}

func (f *fakeAPI) GetRunnerByName(_ context.Context, name string) (*scaleset.RunnerReference, error) {
	f.byNameArg = name
	return f.runner, f.byNameErr
}

func (f *fakeAPI) RemoveRunner(_ context.Context, id int64) error {
	f.removedID = id
	return f.removeErr
}

func (f *fakeAPI) MessageSessionClient(_ context.Context, id int, owner string, _ ...scaleset.HTTPOption) (*scaleset.MessageSessionClient, error) {
	f.sessionArg = []any{id, owner}
	return f.session, f.sessionErr
}

func newTestClient(f *fakeAPI) Client {
	return newClient(f, slog.New(slog.DiscardHandler))
}

// libErr 는 라이브러리 newRequestResponseError 가 만드는 문자열 형식을 흉내 낸다.
func libErr(status string, wrapped error) error {
	return fmt.Errorf("request DELETE https://x/_apis/x failed(status=%q, activity_id=\"a\"): %w: body", status, wrapped)
}

// --- GetRunner [§8.3] ---

// TestCheckAuth_S7_1_2: 시작 시 인증·scope 확인은 runner group 조회 한 번이다. R7 의 `Default` 는
// 클라이언트 상수로 정규화되고 그 밖의 이름은 그대로 나가며, 실패는 그대로 올라가 시작 실패가 된다
// (라이브러리는 그룹 미존재도 오류로 돌려준다: v0.4.0 client.go count 0 → error). [§7.1-2, R7]
func TestCheckAuth_S7_1_2(t *testing.T) {
	f := &fakeAPI{group: &scaleset.RunnerGroup{ID: 1}}
	if err := newClient(f, nil).CheckAuth(context.Background(), "Default"); err != nil {
		t.Fatalf("CheckAuth: %v", err)
	}
	if f.groupArg != scaleset.DefaultRunnerGroup {
		t.Fatalf("groupArg %q, want %q", f.groupArg, scaleset.DefaultRunnerGroup)
	}

	bad := &fakeAPI{groupErr: errors.New("401 Unauthorized")}
	err := newClient(bad, nil).CheckAuth(context.Background(), "Default")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want 401 전달", err)
	}

	custom := &fakeAPI{group: &scaleset.RunnerGroup{ID: 2}}
	if err := newClient(custom, nil).CheckAuth(context.Background(), "custom"); err != nil {
		t.Fatalf("CheckAuth(custom): %v", err)
	}
	if custom.groupArg != "custom" { // Default 가 아닌 이름은 그대로 나간다
		t.Fatalf("groupArg %q, want %q", custom.groupArg, "custom")
	}
}

func TestGetRunner_S8_3_NilNilIsNotFound(t *testing.T) {
	f := &fakeAPI{}
	ref, found, err := newTestClient(f).GetRunner(context.Background(), "ss-m-u")
	if err != nil || found {
		t.Fatalf("want found=false err=nil, got found=%v err=%v", found, err)
	}
	if ref != (RunnerRef{}) {
		t.Fatalf("want zero ref, got %+v", ref)
	}
	if f.byNameArg != "ss-m-u" {
		t.Fatalf("GetRunnerByName arg = %q", f.byNameArg)
	}
}

func TestGetRunner_S8_3_Found(t *testing.T) {
	f := &fakeAPI{runner: &scaleset.RunnerReference{ID: 42, Name: "ss-m-u"}}
	ref, found, err := newTestClient(f).GetRunner(context.Background(), "ss-m-u")
	if err != nil || !found {
		t.Fatalf("want found=true err=nil, got found=%v err=%v", found, err)
	}
	if ref != (RunnerRef{ID: 42, Name: "ss-m-u"}) {
		t.Fatalf("ref = %+v", ref)
	}
}

func TestGetRunner_S8_3_ErrorPropagates(t *testing.T) {
	f := &fakeAPI{byNameErr: errors.New("boom")}
	_, found, err := newTestClient(f).GetRunner(context.Background(), "x")
	if err == nil || found {
		t.Fatalf("want error and found=false, got found=%v err=%v", found, err)
	}
}

// --- RemoveRunner [§8.3] ---

func TestRemoveRunner_S8_3_NotFoundIsNil(t *testing.T) {
	f := &fakeAPI{removeErr: libErr("404 Not Found", scaleset.RunnerNotFoundError)}
	if err := newTestClient(f).RemoveRunner(context.Background(), 9); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
	if f.removedID != 9 {
		t.Fatalf("RemoveRunner id = %d", f.removedID)
	}
}

func TestRemoveRunner_S8_3_OtherErrorPropagates(t *testing.T) {
	want := libErr("400 Bad Request", scaleset.JobStillRunningError)
	f := &fakeAPI{removeErr: want}
	err := newTestClient(f).RemoveRunner(context.Background(), 9)
	if !errors.Is(err, scaleset.JobStillRunningError) {
		t.Fatalf("want wrapped JobStillRunningError, got %v", err)
	}
}

// --- IsBusy [§7.2-3] ---

func TestIsBusy_S7_2_3(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"job still running sentinel", libErr("400 Bad Request", scaleset.JobStillRunningError), true},
		{"bare sentinel", scaleset.JobStillRunningError, true},
		{"runner not found sentinel", libErr("404 Not Found", scaleset.RunnerNotFoundError), false},
		{"4xx generic", libErr("409 Conflict", errors.New("unexpected status code: 409")), true},
		{"403 forbidden", libErr("403 Forbidden", errors.New("unexpected status code: 403")), true},
		{"404 without sentinel", libErr("404 Not Found", errors.New("unexpected status code: 404")), false},
		{"5xx", libErr("503 Service Unavailable", errors.New("unexpected status code: 503")), false},
		{"transport error", errors.New("dial tcp: connection refused"), false},
		{"wrapped by caller", fmt.Errorf("remove runner 9: %w", libErr("400 Bad Request", errors.New("unexpected status code: 400"))), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsBusy(c.err); got != c.want {
				t.Fatalf("IsBusy(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// --- EnsureScaleSet / DeleteScaleSet [§7.1-5, §11, R7] ---

func TestEnsureScaleSet_R7_DefaultCaseInsensitive(t *testing.T) {
	for _, in := range []string{"Default", "default", "DEFAULT"} {
		f := &fakeAPI{group: &scaleset.RunnerGroup{ID: 1}, set: &scaleset.RunnerScaleSet{ID: 5}}
		id, err := newTestClient(f).EnsureScaleSet(context.Background(), "ss", in)
		if err != nil || id != 5 {
			t.Fatalf("%q: id=%d err=%v", in, id, err)
		}
		if f.groupArg != scaleset.DefaultRunnerGroup {
			t.Fatalf("%q: GetRunnerGroupByName arg = %q, want %q", in, f.groupArg, scaleset.DefaultRunnerGroup)
		}
	}
}

func TestEnsureScaleSet_S7_1_5_NonDefaultGroupPassedThrough(t *testing.T) {
	f := &fakeAPI{group: &scaleset.RunnerGroup{ID: 3}, set: &scaleset.RunnerScaleSet{ID: 5}}
	if _, err := newTestClient(f).EnsureScaleSet(context.Background(), "ss", "Linux-Team"); err != nil {
		t.Fatal(err)
	}
	if f.groupArg != "Linux-Team" {
		t.Fatalf("group arg = %q", f.groupArg)
	}
	if f.getArgs[0] != 3 || f.getArgs[1] != "ss" {
		t.Fatalf("GetRunnerScaleSet args = %v", f.getArgs)
	}
}

func TestEnsureScaleSet_S7_1_5_ExistingIsReused(t *testing.T) {
	f := &fakeAPI{group: &scaleset.RunnerGroup{ID: 1}, set: &scaleset.RunnerScaleSet{ID: 5}}
	id, err := newTestClient(f).EnsureScaleSet(context.Background(), "ss", "Default")
	if err != nil || id != 5 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	if f.created != nil {
		t.Fatal("must not create when the scale set exists")
	}
}

func TestEnsureScaleSet_S7_1_5_CreatesWhenMissing(t *testing.T) {
	f := &fakeAPI{group: &scaleset.RunnerGroup{ID: 1}}
	id, err := newTestClient(f).EnsureScaleSet(context.Background(), "ss", "Default")
	if err != nil || id != 77 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	c := f.created
	if c == nil {
		t.Fatal("CreateRunnerScaleSet not called")
	}
	if c.Name != "ss" || c.RunnerGroupID != 1 {
		t.Fatalf("created = %+v", c)
	}
	// runs-on 라벨은 scale set 이름 하나뿐이다. [§4.2, §11]
	if len(c.Labels) != 1 || c.Labels[0].Name != "ss" {
		t.Fatalf("labels = %+v", c.Labels)
	}
	if !c.RunnerSetting.DisableUpdate {
		t.Fatal("DisableUpdate must be set: runner version is pinned by the image")
	}
}

func TestEnsureScaleSet_S7_1_5_GroupLookupError(t *testing.T) {
	f := &fakeAPI{groupErr: errors.New("no runner group found")}
	if _, err := newTestClient(f).EnsureScaleSet(context.Background(), "ss", "Default"); err == nil {
		t.Fatal("want error")
	}
	if f.getArgs != nil || f.created != nil {
		t.Fatal("must stop after the group lookup fails")
	}
}

func TestDeleteScaleSet_S11_SameLookupPath(t *testing.T) {
	f := &fakeAPI{group: &scaleset.RunnerGroup{ID: 2}, set: &scaleset.RunnerScaleSet{ID: 9}}
	if err := newTestClient(f).DeleteScaleSet(context.Background(), "ss", "default"); err != nil {
		t.Fatal(err)
	}
	if f.groupArg != scaleset.DefaultRunnerGroup || f.getArgs[0] != 2 || f.getArgs[1] != "ss" {
		t.Fatalf("lookup args = %q %v", f.groupArg, f.getArgs)
	}
	if f.deletedID != 9 {
		t.Fatalf("deleted id = %d", f.deletedID)
	}
}

func TestDeleteScaleSet_S11_MissingIsNotFound(t *testing.T) {
	f := &fakeAPI{group: &scaleset.RunnerGroup{ID: 2}}
	err := newTestClient(f).DeleteScaleSet(context.Background(), "ss", "Default")
	if !errors.Is(err, ErrScaleSetNotFound) {
		t.Fatalf("want ErrScaleSetNotFound, got %v", err)
	}
	if f.deletedID != 0 {
		t.Fatal("must not call delete")
	}
}

// --- GenerateJIT [§7.2-3, §7.2-4] ---

func TestGenerateJIT_S7_2_3_SettingAndRef(t *testing.T) {
	f := &fakeAPI{jit: &scaleset.RunnerScaleSetJitRunnerConfig{
		Runner:           &scaleset.RunnerReference{ID: 11, Name: "ss-m-u"},
		EncodedJITConfig: "ENCODED",
	}}
	enc, ref, err := newTestClient(f).GenerateJIT(context.Background(), 5, "ss-m-u")
	if err != nil {
		t.Fatal(err)
	}
	if enc != "ENCODED" || ref != (RunnerRef{ID: 11, Name: "ss-m-u"}) {
		t.Fatalf("enc=%q ref=%+v", enc, ref)
	}
	if f.jitSetID != 5 || f.jitSetting.Name != "ss-m-u" || f.jitSetting.WorkFolder != "/home/runner/_work" {
		t.Fatalf("setting = %+v id=%d", f.jitSetting, f.jitSetID)
	}
}

func TestGenerateJIT_S7_2_3_MissingRunnerIsError(t *testing.T) {
	f := &fakeAPI{jit: &scaleset.RunnerScaleSetJitRunnerConfig{EncodedJITConfig: "ENCODED"}}
	_, _, err := newTestClient(f).GenerateJIT(context.Background(), 5, "ss-m-u")
	if err == nil {
		t.Fatal("want error when the response carries no runner reference")
	}
	// JIT 값은 오류 문자열에도 남지 않는다. [§7.2-4]
	if strings.Contains(err.Error(), "ENCODED") {
		t.Fatalf("error leaks JIT config: %v", err)
	}
}

func TestGenerateJIT_S7_2_4_EmptyConfigIsError(t *testing.T) {
	f := &fakeAPI{jit: &scaleset.RunnerScaleSetJitRunnerConfig{Runner: &scaleset.RunnerReference{ID: 1, Name: "n"}}}
	if _, _, err := newTestClient(f).GenerateJIT(context.Background(), 5, "n"); err == nil {
		t.Fatal("want error on empty encoded config")
	}
}

// --- NewSession [§7.1-9, DESIGN §4.4] ---

func TestNewSession_S7_1_9_ErrorPropagates(t *testing.T) {
	f := &fakeAPI{sessionErr: errors.New("boom")}
	_, err := newTestClient(f).NewSession(context.Background(), 5, "owner")
	if err == nil {
		t.Fatal("want error")
	}
	if f.sessionArg[0] != 5 || f.sessionArg[1] != "owner" {
		t.Fatalf("session args = %v", f.sessionArg)
	}
}

// *scaleset.MessageSessionClient 가 Session(listener.Client + Close)을 만족하는지 컴파일 타임에 고정한다. [DESIGN §4.4]
var _ Session = (*scaleset.MessageSessionClient)(nil)
var _ listener.Client = (*scaleset.MessageSessionClient)(nil)

// *scaleset.Client 가 래퍼가 요구하는 api 를 만족하는지 고정한다. [DESIGN §4.4]
var _ api = (*scaleset.Client)(nil)
