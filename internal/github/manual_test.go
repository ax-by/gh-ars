//go:build manual

package github

// 실제 GitHub 에 대한 수동 확인이다(PLAN Phase 6 완료 기준). 네트워크와 PAT 가 필요하므로
// `manual` 태그로 격리한다. 실행:
//
//	$env:GH_ARS_MANUAL_URL="https://github.com/OWNER/REPO"; $env:GH_ARS_MANUAL_TOKEN="<PAT>"
//	.\scripts\go.ps1 test -tags manual -run TestManual -v ./internal/github
//
// 순서: EnsureScaleSet(생성) → EnsureScaleSet(재사용, 같은 id) → GenerateJIT → GetRunner(found)
// → RemoveRunner → GetRunner(not found) → RemoveRunner(재호출, nil) → NewSession → DeleteScaleSet
// → DeleteScaleSet(ErrScaleSetNotFound). scale set 이름은 실행마다 유일하다.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"gh-ars/internal/config"
)

func TestManual_S7_1_5_S8_3_S11_RealRepo(t *testing.T) {
	url, token := os.Getenv("GH_ARS_MANUAL_URL"), os.Getenv("GH_ARS_MANUAL_TOKEN")
	if url == "" || token == "" {
		t.Skip("GH_ARS_MANUAL_URL / GH_ARS_MANUAL_TOKEN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	c, err := New(config.GitHub{URL: url, Auth: config.Auth{Token: token}}, log)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("gh-ars-manual-%d", time.Now().Unix())
	defer func() {
		if err := c.DeleteScaleSet(context.Background(), name, config.DefaultRunnerGroup); err != nil && !errors.Is(err, ErrScaleSetNotFound) {
			t.Logf("cleanup: %v", err)
		}
	}()

	id, err := c.EnsureScaleSet(ctx, name, config.DefaultRunnerGroup)
	if err != nil {
		t.Fatalf("ensure (create): %v", err)
	}
	id2, err := c.EnsureScaleSet(ctx, name, config.DefaultRunnerGroup)
	if err != nil || id2 != id {
		t.Fatalf("ensure (reuse): id=%d id2=%d err=%v", id, id2, err)
	}

	runnerName := name + "-m-01ARZ3NDEKTSV4RRFFQ69G5FAV"
	enc, ref, err := c.GenerateJIT(ctx, id, runnerName)
	if err != nil {
		t.Fatalf("jit: %v", err)
	}
	t.Logf("jit: runnerID=%d name=%s len=%d", ref.ID, ref.Name, len(enc))
	if ref.Name != runnerName {
		t.Fatalf("runner name = %q, want %q", ref.Name, runnerName)
	}

	got, found, err := c.GetRunner(ctx, runnerName)
	if err != nil || !found || got.ID != ref.ID {
		t.Fatalf("get after jit: ref=%+v found=%v err=%v", got, found, err)
	}
	if err := c.RemoveRunner(ctx, ref.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, found, err := c.GetRunner(ctx, runnerName); err != nil || found {
		t.Fatalf("get after remove: found=%v err=%v", found, err)
	}
	if err := c.RemoveRunner(ctx, ref.ID); err != nil {
		t.Fatalf("remove again (RunnerNotFoundError → nil): %v", err)
	}

	sess, err := c.NewSession(ctx, id, "gh-ars-manual")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	t.Logf("session: id=%s", sess.Session().SessionID)
	if err := sess.Close(ctx); err != nil {
		t.Fatalf("session close: %v", err)
	}

	if err := c.DeleteScaleSet(ctx, name, config.DefaultRunnerGroup); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := c.DeleteScaleSet(ctx, name, config.DefaultRunnerGroup); !errors.Is(err, ErrScaleSetNotFound) {
		t.Fatalf("delete again: want ErrScaleSetNotFound, got %v", err)
	}
}
