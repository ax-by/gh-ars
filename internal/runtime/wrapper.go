package runtime

import (
	"fmt"

	"gh-ars/internal/domain"
)

// jitEnvVar 는 runner 가 --jitconfig 인자와 동등하게 읽는 환경변수다. 래퍼가 파일에서
// 읽어 export 하므로 gh-ars 는 이 이름을 컨테이너 env 에 절대 넣지 않는다. [§7.2-4]
const jitEnvVar = "ACTIONS_RUNNER_INPUT_JITCONFIG"

// 모드별 entrypoint 래퍼. SPEC §7.2-4 의 문자열을 그대로 둔다. 파일을 읽어 export 하고
// 즉시 삭제한 뒤 exec 하므로 값이 argv·컨테이너 env·디스크에 남지 않는다. [§7.2-4]
const (
	wrapperNone = `IFS= read -r ACTIONS_RUNNER_INPUT_JITCONFIG < /home/runner/.jitconfig && rm -f /home/runner/.jitconfig && export ACTIONS_RUNNER_INPUT_JITCONFIG && exec ./run.sh`
	// sidecar 는 read 앞에 소켓 대기(150×0.2s = 30s 상한, 초과 시 exit 1 → die → 정리 → 재배치).
	// `[ -S ]` 만 써서 docker CLI 가 없는 커스텀 이미지도 지원한다. [§7.2-4, §9.1]
	wrapperSidecar = `i=0; until [ -S /var/run/docker.sock ]; do i=$((i+1)); [ $i -ge 150 ] && exit 1; sleep 0.2; done; IFS= read -r ACTIONS_RUNNER_INPUT_JITCONFIG < /home/runner/.jitconfig && rm -f /home/runner/.jitconfig && export ACTIONS_RUNNER_INPUT_JITCONFIG && exec ./run.sh`
)

// wrapperShell 은 래퍼를 실행하는 셸이다. 커스텀 이미지도 bash 를 유지해야 한다. [§9.1]
const wrapperShell = "/bin/bash"

// Wrapper 는 runner 컨테이너의 Entrypoint 와 Cmd 를 돌려준다. [§7.2-4, DESIGN §4.2]
func Wrapper(mode domain.Mode) (entrypoint, cmd []string, err error) {
	var script string
	switch mode {
	case domain.ModeNone:
		script = wrapperNone
	case domain.ModeSidecar:
		script = wrapperSidecar
	default:
		return nil, nil, fmt.Errorf("runtime: 알 수 없는 mode %q", mode)
	}
	return []string{wrapperShell}, []string{"-c", script}, nil
}
