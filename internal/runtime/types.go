// Package runtime 은 docker/podman CLI 를 공통 모델로 정규화한다. [§5, §7, §9]
package runtime

import "time"

// Container 는 `ps -a` 한 줄이다. 이름으로 unit·role 을 식별하고,
// 라벨은 입양 시 scale set/mode 복원에 쓴다. [§4.2, §8.3]
type Container struct {
	Name    string // gh-ars-<unit>-runner|sidecar
	Labels  map[string]string
	State   string // running | exited | created ...
	Created time.Time
}
