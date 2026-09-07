package runtime

import (
	"reflect"
	"strings"
	"testing"

	"gh-ars/internal/domain"
)

// [§7.2-4] 모드별 래퍼 문자열은 SPEC 의 것과 글자 단위로 일치해야 한다. 래퍼가 읽는 경로·
// 변수명·exec 대상이 하나라도 다르면 JIT 전달이 조용히 깨진다. 여기의 기대값은 SPEC 에서
// 그대로 옮긴 리터럴이며 구현 상수를 재사용하지 않는다.
func TestWrapper_S7_2_4_MatchesSpec(t *testing.T) {
	const specNone = `IFS= read -r ACTIONS_RUNNER_INPUT_JITCONFIG < /home/runner/.jitconfig && rm -f /home/runner/.jitconfig && export ACTIONS_RUNNER_INPUT_JITCONFIG && exec ./run.sh`
	const specSidecar = `i=0; until [ -S /var/run/docker.sock ]; do i=$((i+1)); [ $i -ge 150 ] && exit 1; sleep 0.2; done; IFS= read -r ACTIONS_RUNNER_INPUT_JITCONFIG < /home/runner/.jitconfig && rm -f /home/runner/.jitconfig && export ACTIONS_RUNNER_INPUT_JITCONFIG && exec ./run.sh`

	for _, tc := range []struct {
		mode domain.Mode
		want string
	}{
		{domain.ModeNone, specNone},
		{domain.ModeSidecar, specSidecar},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			ep, cmd, err := Wrapper(tc.mode)
			if err != nil {
				t.Fatalf("Wrapper: %v", err)
			}
			if !reflect.DeepEqual(ep, []string{"/bin/bash"}) {
				t.Fatalf("entrypoint = %v, want [/bin/bash]", ep)
			}
			if !reflect.DeepEqual(cmd, []string{"-c", tc.want}) {
				t.Fatalf("cmd = %q, want [-c %q]", cmd, tc.want)
			}
		})
	}
}

// [§7.2-4] 래퍼는 mode 별로 둘뿐이다. 모르는 mode 로 컨테이너를 만들지 않는다.
func TestWrapper_S7_2_4_UnknownMode(t *testing.T) {
	if _, _, err := Wrapper(domain.Mode("bogus")); err == nil {
		t.Fatal("알 수 없는 mode 가 오류가 아니다")
	}
}

// [§7.2-4] 래퍼가 읽는 경로는 jittar 가 놓는 경로(RunnerHome/.jitconfig)여야 한다.
func TestWrapper_S7_2_4_PathMatchesJITTar(t *testing.T) {
	_, cmd, _ := Wrapper(domain.ModeNone)
	want := "< " + RunnerHome + "/" + jitFileName + " "
	if !strings.Contains(cmd[1], want) {
		t.Fatalf("래퍼가 %q 를 읽지 않는다: %q", want, cmd[1])
	}
	if !strings.Contains(cmd[1], "export "+jitEnvVar) {
		t.Fatalf("래퍼가 %s 를 export 하지 않는다", jitEnvVar)
	}
}
