# PLAN.md — 진행 상태

역할: 무엇을 어떤 순서로 만들고 어디까지 왔는지. 규범은 `docs/SPEC.md`, 구조는 `docs/DESIGN.md`, 검증 절차는 `docs/TESTPLAN.md`, 결정 근거는 `docs/DECISIONS.md`.

지시 단위는 **Phase 번호**다("Phase 3 구현해줘"). `gate.ps1 -Phase N`, `commit.ps1 -Phase N`도 같은 번호를 쓴다.

## 사전 준비 (Phase 1 전)

- 루프 게이트(`scripts/gate.ps1`, `scripts/commit.ps1`, `.loop/state.json`, 직접 커밋 거부 훅, SessionStart 요약 훅) — 완료 2026-09-06. 이 작업 자체는 게이트를 거치지 않았다.
- Phase 0 체크리스트 작성 — 완료(아래 절). 결과 수신은 Phase 9(podman) 시작 전까지 필요하다(`podman cp -` 실패 시 SPEC §7.2-4의 "원격 디스크에 남지 않음" 요구가 바뀐다). 0-2(slice)는 Phase 12 전까지.
- 첫 커밋: `git init`, `.gitignore` — 완료(91018ed).
- go.mod 외부 의존성은 사전 준비가 아니다. **처음 쓰는 Phase에서 그 Phase 커밋에 함께 넣는다**(도입 Phase 표는 DESIGN §10). 그래서 go.mod가 비어 있어도 밀린 작업이 아니다.

## Phase 표

각 Phase는 이전 Phase의 테스트가 통과한 뒤 시작한다. 완료 판정은 `docs/TESTPLAN.md` §1에서 해당 태그가 붙은 항목 전부 통과. E2E 번호는 TESTPLAN §2. 상태 칸은 Phase 4 이상에서 `commit.ps1`이 갱신하고, Phase 1~3은 사람이 커밋한 뒤 에이전트가 갱신한다. Phase 0은 사람이 실행한다.

| Phase | 범위 | SPEC | DESIGN | 완료 기준 | 상태 |
|---|---|---|---|---|---|
| 0 | 스파이크: 미확정 동작 검증 | §7.2-4, §9.3 | – | 아래 절 4항목 결과 기록 | 완료 |
| 1 | `resource`, `domain` | §4, §8.1, §9.3 표, R25 | §3.1, §3.4 | TESTPLAN (resource), (domain) | 완료 2026-09-06 (게이트 3회차 PASS, findings=0. 커밋 73c497c 문서 / 92a4abc 코드) |
| 2 | `config` | §6 전체, R2~R15·R17~R20·R23 필수성·R25 | §8 | TESTPLAN (config). `examples/gh-ars.yaml` 로드 성공 | 완료 2026-09-06 (게이트 4회차 PASS, findings=1 doc-gap만 잔존. 커밋 06f4baf. yaml.v3 도입) |
| 3 | `plan` | §7.2-3, §8.1~§8.3 | §5, §3.2 집계 정의 | TESTPLAN (plan) | 완료 2026-09-07 (게이트 2회차 PASS, 지적 반영·문서 동기화로 4회 더 실행해 최종 findings=0. 커밋 2795e2e 문서 / bcd81fa 코드. `plan`이 타입으로 참조하는 `runtime.Container`만 선행 추가) |
| 4 | `executor/local` | §5 Executor, §10.2(local 사용자 권한) | §4.1 | TESTPLAN (executor/local) | 완료 2026-09-07 |
| 5 | `runtime/docker` + `jittar` | §7.2-4 none 래퍼·tar, §9.1 볼륨·라벨, §4.2 라벨 | §4.2 | TESTPLAN (runtime/docker), (runtime/jittar). 실제 docker로 수동 확인 | 미착수 |
| 6 | `github` | §7.1-5, §7.2-1·2·4, §8.3 등록 처리, §11, R7 | §4.4, §4.5 | TESTPLAN (github). 실제 repo에 ensure/JIT/GetRunner/RemoveRunner/delete 수동 확인 | 미착수 |
| 7 | `controller` 최소(none, local, docker) + `cmd` — walking skeleton | §7 전체(none·local 범위), §8.3 정리 순서, §11 | §6, §9, §4.5 | TESTPLAN (controller/core). E2E-lite: 실제 repo에서 job 1개 완주(사람 실행) | 미착수 |
| 8 | `executor/ssh` | §10.1, R20 | §4.1 | TESTPLAN (executor/ssh) | 미착수 |
| 9 | `runtime/podman` | §9.2 podman info 형식, §10.2 규칙 3 경로 | §4.2 | TESTPLAN (runtime/podman). Phase 0-3 결과 필요 | 미착수 |
| 10 | `machine` 에이전트 | §7.1-3·4·7·8, §10.2, R16, R21, R22 | §7, §3.3 | TESTPLAN (machine) | 미착수 |
| 11 | `controller` 확장: 입양·전체 동기화, tick 대조 완성, 축소(Draining), Dying 재시도, R24 | §7.2-3 축소, §8.3 전체, R24 | §6, §3.2 | TESTPLAN (controller/ext). E2E 1, 3, 4, 5 | 미착수 |
| 12 | `systemd` + sidecar(컨테이너·볼륨·slice, docker/podman 변형) | §9 전체, §7.2-4 sidecar 래퍼, §10.2 sidecar 행 | §4.2 sidecar 표, §4.3, §6 startUnit | TESTPLAN (systemd), (runtime/sidecar). E2E 2. Phase 0-2 결과 필요 | 미착수 |

## Phase 0 스파이크 (Linux 머신에서 실행)

목적: 코드 전에 SPEC §7.2-4·§9.3의 전제를 확인한다. 실패하면 `docs/TESTPLAN.md` §2 표의 대안을 적용하고 SPEC을 먼저 고친다. 결과 칸에 "통과 / 실패(출력 요지)"를 적는다. Phase 1~8과 병렬로 진행해도 된다.

### 0-1a. 래퍼 메커니즘 (GitHub 불필요, docker 머신)
```bash
docker create --name t1 --entrypoint /bin/bash ghcr.io/actions/actions-runner:2.337.0 -c \
  'IFS= read -r ACTIONS_RUNNER_INPUT_JITCONFIG < /home/runner/.jitconfig && rm -f /home/runner/.jitconfig && export ACTIONS_RUNNER_INPUT_JITCONFIG && echo "len=${#ACTIONS_RUNNER_INPUT_JITCONFIG}" && ls /home/runner/.jitconfig; sleep 30'
printf 'hello-jit\n' > .jitconfig
tar -cf - --owner=1001 --group=1001 --mode=0600 .jitconfig | docker cp - t1:/home/runner
docker start t1 && sleep 2 && docker logs t1
docker inspect -f '{{.Config.Env}} {{.Config.Cmd}}' t1 | grep -c hello-jit
docker rm -f t1; rm -f .jitconfig
```
기대: logs에 `len=9`와 `ls: cannot access ... No such file`, grep 결과 `0`. 결과: 통과 (2026-09-06, WSL2 Ubuntu + Docker Desktop 통합, docker 28.x). len=9, ls 실패, grep count 0 모두 일치.

### 0-1b. JIT 등록 (repo scope, `gh` 로그인 필요. OWNER/REPO 치환)
```bash
JIT=$(gh api -X POST repos/OWNER/REPO/actions/runners/generate-jitconfig \
  -f name=spike-1 -F runner_group_id=1 -f 'labels[]=spike' --jq .encoded_jit_config)
docker create --name t2 --entrypoint /bin/bash ghcr.io/actions/actions-runner:2.337.0 -c \
  'IFS= read -r ACTIONS_RUNNER_INPUT_JITCONFIG < /home/runner/.jitconfig && rm -f /home/runner/.jitconfig && export ACTIONS_RUNNER_INPUT_JITCONFIG && exec ./run.sh'
printf '%s\n' "$JIT" > .jitconfig && tar -cf - --owner=1001 --group=1001 --mode=0600 .jitconfig | docker cp - t2:/home/runner && rm -f .jitconfig
docker start t2 && sleep 15
gh api repos/OWNER/REPO/actions/runners --jq '.runners[] | select(.name=="spike-1") | .status'
docker exec t2 test ! -e /home/runner/.jitconfig && echo no-file
docker inspect -f '{{.Config.Env}}' t2 | grep -c ACTIONS_RUNNER_INPUT
docker rm -f t2
```
기대: status `online`, `no-file`, grep `0`. 등록은 GitHub이 1일 내 자동 제거(SPEC §3.2). 결과: 통과 (2026-09-06). status=online, no-file, grep count 0 모두 일치.

### 0-2. 비활성 slice에 set-property (systemd + cgroup v2 + docker cgroup driver systemd)
```bash
docker info --format '{{.CgroupDriver}} {{.CgroupVersion}}'          # 기대: systemd 2
sudo systemctl set-property --runtime gh-ars-spike.slice CPUQuota=50% MemoryMax=256M
docker run -d --name t3 --cgroup-parent=gh-ars-spike.slice alpine sleep 300
systemctl show gh-ars-spike.slice -p CPUQuotaPerSecUSec -p MemoryMax    # 기대: 500ms, 268435456
cat /sys/fs/cgroup/gh-ars-spike.slice/cpu.max                          # 기대: 50000 100000
docker rm -f t3; sudo systemctl stop gh-ars-spike.slice; sudo systemctl revert gh-ars-spike.slice
```
결과: 통과, 단 발견사항 있음 (2026-09-06, WSL2 Ubuntu, docker 대신 **podman**으로 대체 실행 — 이 환경의 docker는 Docker Desktop 통합이라 cgroup driver가 `cgroupfs`라 전제 불충족. podman은 `cgroupManager=systemd, cgroupVersion=v2`로 전제 충족). CPUQuotaPerSecUSec=500ms, MemoryMax=268435456, cpu.max=`50000 100000`, memory.max=`268435456` 모두 기대값과 일치.
**발견**: 실제 cgroup 경로는 스크립트가 가정한 flat 경로(`/sys/fs/cgroup/gh-ars-spike.slice`)가 아니라, systemd가 슬라이스 이름의 대시(`-`)를 계층 구분자로 해석해 중첩 경로(`/sys/fs/cgroup/gh.slice/gh-ars.slice/gh-ars-spike.slice`)를 만든다. Phase 12에서 slice 이름 규칙을 정할 때 이 중첩을 고려해야 한다(코드가 cgroup 경로를 직접 읽는 부분이 있다면 특히).

### 0-3. `podman cp -` stdin tar (podman 머신)
```bash
podman create --name t4 alpine sleep 60
printf 'x\n' > f && tar -cf - f | podman cp - t4:/tmp && rm -f f
podman start t4 && podman exec t4 cat /tmp/f                              # 기대: x
podman rm -f t4
```
결과: 통과 (2026-09-06, WSL2 Ubuntu, podman 5.7.0). `cat /tmp/f` 결과 `x`, 기대값과 일치.

## E2E 결과 (docs/TESTPLAN.md §2)

| # | 시나리오 | 결과 | 일자 / 비고 |
|---|---|---|---|
| lite | Phase 7: local docker 머신 1대, none 모드 job 1개 완주 | 미실행 | |
| 1 | none 모드 3대 spread + JIT 미노출 + 정리 순서 | 미실행 | |
| 2 | sidecar docker/podman, docker build·container:·services: | 미실행 | |
| 3 | 재시작 후 입양, 중복 생성 없음 | 미실행 | |
| 4 | SSH 단절 → 배치 제외·capacity 감소 | 미실행 | |
| 5 | scaleset delete | 미실행 | |

## 남은 결정

구현 시 기본값으로 정하기로 한 항목(URL 판별 규칙, `${file:}` 공백 제거, 다이제스트 참조 허용, `RunnerSetting{Ephemeral, DisableUpdate}`, 세션 owner 문자열)은 코드 주석에 근거를 남긴다.

- R13 다이제스트 예외의 SPEC 명시 여부 (Phase 2 리뷰 doc-gap). 구현은 유효한 `@sha256:<64 hex>`/`@sha512:<128 hex>` 참조를 태그와 무관하게 허용한다(`internal/config/resolve.go` checkImage). SPEC R13에 한 줄 추가할지 사람이 결정.
- `scripts/codex-review.ps1`의 node 래퍼가 Codex 완료 후 종료하지 않는 문제 (Phase 2 게이트 매 회차 재현). 완료 후 타임아웃 또는 세션 로그의 `task_complete` 감지가 후속 과제.
