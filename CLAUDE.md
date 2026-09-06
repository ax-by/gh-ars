# gh-ars

K8s 없이 정적 머신(SSH 또는 local) 위에서 docker/podman 컨테이너로 GitHub Actions ephemeral runner를 오토스케일링하는 Go CLI. GitHub 통신은 `github.com/actions/scaleset`(listener 패키지 포함).

| 파일 | 역할 |
|---|---|
| `docs/SPEC.md` | 규범. 무엇이 맞는가. 규칙 R2~R25(R1은 삭제됨), 절 §n으로 참조. 충돌 시 우선 |
| `docs/DESIGN.md` | 코드 구조. 패키지·인터페이스·상태 전이 |
| `PLAN.md` | 진행 상태. Phase 0~12 표(범위·SPEC/DESIGN 절·완료 기준·상태), 스파이크 체크리스트, E2E 결과 |
| `docs/TESTPLAN.md` | 검증 절차. §1 유닛 테스트 필수 항목, §2 수동 E2E 체크리스트 |
| `docs/DECISIONS.md` | 설계 결정 기록. 결정 / 이유 / 기각한 대안. 새 결정은 여기에 행 추가 |
| `examples/gh-ars.yaml` | 설정 파일 예시 |
| `docs/review/PROMPT.md` | 리뷰 계약. 리뷰어에게 주는 축과 형식. 템플릿은 수정하지 않는다 |

코드가 SPEC/DESIGN과 맞지 않으면 **코드를 SPEC에 맞춰 고친다.** SPEC이 틀렸거나 구현 불가라고 판단되면 코드도 문서도 고치지 말고 멈춰서 어느 절이 왜 문제인지 보고한다. 문서를 바꾸기로 결정되면 SPEC → DESIGN → examples 순으로 동기화한다.

## 환경

- Go는 PATH에 없다. `scripts/go.ps1`이 `C:\Users\user\sdk\go1.27.0\bin\go.exe`를 감싼다. 아래의 `go`는 이 래퍼를 뜻한다. `jq` 없음.
- 개발 머신은 Windows, 실행 대상은 Linux 머신이다. 기본 셸은 **PowerShell 5.1**이다(`&&`, `||`, `2>/dev/null` 없음. pwsh 7 없음). bash 문법이 필요하면 Git Bash를 명시적으로 쓴다. 이 리포의 개발 머신용 스크립트(`scripts/*.ps1`, `scripts/hooks/*.ps1`)는 전부 PowerShell로 통일한다.
- 빌드·테스트·게이트:
  ```
  .\scripts\go.ps1 build ./...
  .\scripts\gate.ps1 -Phase N -Packages "internal/x" -Spec "S8.1" -Design "S5" -Base HEAD -TestOnly  # test + vet 만
  .\scripts\gate.ps1 -Phase N -Packages "internal/x" -Spec "S8.1" -Design "S5" -Base HEAD            # + Codex 리뷰, state 기록
  .\scripts\commit.ps1 -Phase N -Message "<type>(<pkg>): ... (Phase N, SPEC §n)"                   # 게이트 통과 시에만
  ```
  절 번호 인자는 `§` 대신 ASCII `S`로 쓴다(`S8.1` = §8.1. 콘솔 코드페이지 때문). 게이트 상태는 `.loop/state.json`(gitignore)에 남는다.
- `.go` 편집 시 gofmt/vet 훅(`scripts/hooks/go-check.ps1`, `.claude/settings.json`에서 exec 형식으로 `powershell.exe` 직접 실행)이 자동으로 돈다. 훅 실패는 즉시 고친다.
- Linux 전용 테스트(`executor/local` 실제 프로세스, 실제 docker)는 `//go:build linux` 태그로 분리한다. 유닛 테스트는 fake Executor·fake GitHub로 OS 무관하게 돌아야 한다. Linux 검증은 당장은 WSL2/VM에서 하고, CI(ubuntu)는 후속 과제다.

## 작업 루프

PLAN.md의 Phase 순서를 따른다. 한 커밋은 Phase 하나(= 패키지 또는 기능 하나).

1. PLAN.md Phase 표에서 그 Phase의 범위·SPEC/DESIGN 절·완료 기준(TESTPLAN 태그)을 읽는다.
2. 테스트를 먼저 쓴다. `docs/TESTPLAN.md` §1에서 그 패키지 태그(예: `(plan)`)가 붙은 항목을 전부 커버한다. 태그가 없는 항목은 만들지 않는다.
3. 구현 중 재테스트는 `gate.ps1 ... -TestOnly`. 구현이 끝나면 `gate.ps1`(리뷰 포함)을 실행하고 출력의 PASS/BLOCK을 그대로 보고한다. 리뷰 원문은 `.codex-review/last.md`.
4. BLOCK이면 지적마다 "반영" 또는 "반박 근거"를 정리해 보고하고, must-fix를 고친 뒤 3번부터. 회차는 gate가 센다. 2회차 후에도 BLOCK이면 멈추고 사람에게 넘긴다(`commit.ps1 -Override "<이유>"`는 사람이 결정한 경우에만).
5. PASS이면 `commit.ps1 -Phase N -Message "<type>(<package>): <요약> (Phase N, SPEC §n)"`. Phase 1~3은 스크립트가 커밋 명령만 출력하므로 사람이 diff를 확인하고 직접 커밋한다. Phase 4~12는 스크립트가 직접 커밋하고 PLAN.md 상태 칸을 갱신한다. `git commit`을 직접 치지 않는다(훅이 거부한다).
6. 커밋 후 코드를 더 바꾸면 게이트는 무효다(트리 해시 불일치). 다시 3번부터.
7. PLAN.md 갱신(Phase 1~3은 사람 커밋 뒤 에이전트가 상태 칸을 고친다).

세션 재개: SessionStart 훅이 `.loop/state.json` 요약을 출력한다. `state.phase`를 이어받되, 실제 진척은 `git status`와 `gate.ps1 ... -TestOnly`로 확인한 뒤 계속한다. 머릿속 기억이 아니라 파일과 git이 진실이다.

E2E(`docs/TESTPLAN.md` §2)는 실제 머신과 GitHub repo가 필요하므로 에이전트가 수행하지 않는다. 해당 단계 끝에 "실행 명령 / 기대 로그 / 확인 명령 / 결과 칸" 형식 체크리스트를 만들어 사람에게 넘기고, 결과를 받아 PLAN.md에 기록한다. E2E 통과를 에이전트가 자기 판단으로 선언하지 않는다.

## 관례

- 테스트 이름에 SPEC 규칙/절 번호: `TestValidate_R15_SidecarRuntimeMismatch`, `TestReconcile_S8_3_ExitedRunner`.
- 코드 주석에 `[§n]` 참조. 규칙을 구현하는 함수에는 반드시.
- 의존 방향: `cmd → {controller, config, logging}`, `controller → {github, machine, plan, domain}`, `config → {domain, resource}`, `plan → {domain, resource}`, `domain → resource`, `machine → {runtime, systemd, executor}`, `runtime → executor`. `plan`·`domain`·`resource`는 I/O를 하지 않는다. 전체 목록은 DESIGN §2.
- 상태는 Controller goroutine만 만진다. 느린 작업은 goroutine으로 빼고 결과를 inbox 메시지로 돌려보낸다. 락으로 우회하지 않는다.
- 상수(grace 5m, 기동 타임아웃 2m, SSH 접속 10s, tick 30s, 백오프 1s→30s)는 SPEC §8.3 표와 §7.1-8을 따르고 한 곳에 모은다. 설정으로 노출하지 않는다.
- 새 외부 의존성은 DESIGN §10 표에 있는 것만. 추가가 필요하면 먼저 보고한다.
- 로그는 `log/slog`. 명령 실패는 argv(secret 제외)와 stderr를 함께 남긴다.
- 한국어로 소통한다. 코드 식별자와 주석은 영어.

## 하지 말 것

- SPEC에 없는 동작을 추가하지 않는다. 필요하면 먼저 SPEC 개정을 제안한다. §3.2 non-goal은 "나중을 위해"도 만들지 않는다.
- JIT config를 로그·argv·컨테이너 env·원격 디스크에 남기지 않는다. 로그에는 길이만.
- 실제 토큰·키·호스트를 코드, 테스트, 예시 설정에 넣지 않는다. 예시는 `${env:...}` 참조로만.
- 네트워크나 실제 docker가 필요한 테스트를 태그 없이 유닛 테스트에 넣지 않는다.
- 테스트를 통과시키려고 테스트를 약화하지 않는다. 구현이 틀렸는지 SPEC이 틀렸는지 먼저 판단해 보고한다.
- 문서를 코드에 맞춰 조용히 수정하지 않는다.
