# gh-ars 설계 문서 (DESIGN.md)

관계: `SPEC.md`가 규범(무엇이 맞는가)의 단일 출처다. 이 문서는 그것을 어떤 코드 구조로 구현하는가를 정한다. 충돌 시 SPEC.md가 이긴다. 각 항목 끝의 `[§n]`은 SPEC.md 참조.

---

## 1. 설계 원칙

1. **순수 로직과 I/O를 분리한다.** 용량·desired·spread·reconcile 판정은 입력 구조체를 받아 결정을 돌려주는 순수 함수다. I/O(GitHub, SSH, 컨테이너)는 인터페이스 뒤에 둔다. [§5]
2. **상태는 단일 goroutine이 소유한다.** 모든 상태 변경(이벤트, GitHub 콜백, 타이머)은 하나의 `Controller` 루프로 직렬화한다. 락 대신 채널.
3. **진실은 머신에 있다.** 메모리 상태는 캐시이며, 전체 동기화로 언제든 재구성할 수 있어야 한다. [§5, §8.3]
4. **actions/scaleset의 `listener`를 그대로 쓴다.** 세션·long-poll·ack·acquire를 재구현하지 않는다. 우리는 `listener.Scaler` 인터페이스만 구현한다.

## 2. 패키지 구조

모듈 경로: `gh-ars` (go.mod). Go 1.27.

```
cmd/gh-ars/                 main. 표준 flag + 서브커맨드 분기 (run / scaleset delete)
internal/config/            YAML strict 디코드, secret 참조 검사, 필드 값 치환, 기본값, 검증 R2~R25       [§6]
internal/resource/          cpu(float64)·memory(bytes) 파싱, systemd/docker 플래그 변환                [§8.1, §9.3]
internal/domain/            Unit, Machine, ScaleSet 모델과 상태 열거형, 이름·라벨 규칙                [§4]
internal/plan/              순수 함수: Capacity, Desired, Spread, Reconcile 판정표                     [§8]
internal/executor/          Executor 인터페이스 + local, ssh 구현                                       [§5, §10]
internal/runtime/           Runtime 인터페이스 + docker, podman 구현, JIT tar 생성(jittar.go)            [§5, §7, §9]
internal/systemd/           slice set-property / stop / revert / Check (Executor 위)                    [§9.3, §10.2]
internal/github/            actions/scaleset 래핑: 클라이언트 생성, scale set ensure/delete, JIT, runner  [§7.1-5, §7.2, §11]
internal/machine/           Machine 에이전트: preflight, pre-pull, events 스트림, 백오프, health          [§7.1, §10]
internal/controller/        Controller: 상태 소유 goroutine, listener.Scaler 구현, unit 생명주기        [§7.2, §8.3]
internal/logging/           slog 설정 (--log-level, --log-format)                                       [§11]
```

의존 방향: `cmd → {controller, config, logging}`, `controller → {github, machine, plan, domain}`, `config → {domain, resource}`, `plan → {domain, resource, runtime(타입만)}`, `domain → resource`, `machine → {runtime, systemd, executor}`, `runtime → executor`. `plan`, `domain`, `resource`는 I/O를 수행하지 않는다(`plan`이 `runtime.Container` 타입을 참조하는 것은 허용, 함수 호출은 금지).

## 3. 도메인 모델

### 3.1 타입  [§4]

```go
package domain

type UnitID string            // ULID, 26자
type Mode string              // "none" | "sidecar"
type RuntimeKind string       // "docker" | "podman"

type Unit struct {
    ID         UnitID
    ScaleSet   string
    Machine    string          // 소속 머신 (권위: 도달한 머신)
    Mode       Mode
    State      UnitState
    CreatedAt  time.Time       // grace·기동 타임아웃 기준
    Adopted    bool            // 재시작 입양 여부 (로그용)
    Foreign    bool            // 라벨의 scale set이 YAML에 없음 → 머신 slot 1개로 계산 [§8.3]
    RunnerName string          // <scaleSet>-<machine>-<unit>. GetRunnerByName 키 [§4.3]
    Busy       bool            // JobStarted 수신. 축소 후보 제외용 최적화일 뿐 정확성 근거가 아님 [§7.2-3]
    Parts      Parts           // 존재하는 부품 집합
}

// UnitState: Creating | Starting | Running | Draining | Dying | Removed

type Parts struct {
    Runner, Sidecar bool
    Volumes         [3]bool    // work, sock, externals
    Slice           bool
}

type Machine struct {
    Name          string
    ScaleSet      string
    Runtime       RuntimeKind
    Local         bool
    Sudo          bool         // podman/systemctl 명령에 sudo -n 접두 여부. preflight 판단 규칙 결과 [§10.2]
    Health        Health       // Healthy | Unhealthy | Failed(시작 실패 사유)
    PhysicalMax   int
    EffectiveMax  int
    LastPlacedAt  time.Time    // spread tie-break ②
}

type ScaleSet struct {
    Name         string
    GitHubID     int
    RunnerGroup  string
    MinRunners   int
    MaxRunners   int
    Unit         resource.Budget   // cpu, memory
    Mode         Mode
    RunnerImage  string
    SidecarImage string            // mode=sidecar 일 때만
    Machines     []string
}
```

### 3.2 Unit 상태 전이  [§7.2, §8.3]

```
             create→cp→start 성공          GitHub 등록 확인 (tick)         축소: RemoveRunner 성공
  Creating ───────────────────────▶ Starting ───────────────────▶ Running ─────────────────────▶ Draining
     │ 기동 타임아웃 2분 / 명령 실패           │ grace 5분 초과 (tick)      │ tick 미등록 (즉시)   ◀──── RemoveRunner 거절(busy) ┘   │ die
     │ 또는 die 도착                          │ 또는 die 도착              │ 또는 runner die                                        │
     ▼                                        ▼                          ▼                                                        │
  Dying ◀─────────────────────────────────────┴──────────────────────────┴────────────────────────────────────────────────────────┘
     │ 정리 순서(아래) 실행. 실패 시 Dying 유지, tick마다 재시도 [§8.3]
     ▼
  Removed (상태에서 삭제)
```

- `Creating`: unit id 발급·JIT 생성 후 컨테이너 명령 실행 중. **running 집계에 포함**된다. [§7.2-3]
  `Creating` 중 `die`가 도착하면 즉시 `Dying`으로 보내고, 나중에 오는 `msgUnitStarted`는 무시한다.
- `Starting`: runner 컨테이너 `running`, GitHub 등록 대기. running 집계 포함.
- `Running`: 등록 확인. `Busy`는 `JobStarted` 수신 시 true. tick 대조에서 미등록이면 grace 없이 `Dying`. [§8.3]
- `Draining`: 축소 대상으로 `RemoveRunner`를 호출한 상태. running 집계 포함. tick의 미등록 판정에서 **제외**. `die`가 오면 `Dying`. `RemoveRunner`가 거절(busy)되면 `Running`으로 복귀. 컨테이너를 직접 rm하지 않는다. [§7.2-3]
- `Dying`: runner 컨테이너 `die` 수신 또는 미등록 판정. 정리 대기. running 집계에서 제외되지만 **부품이 남아 있는 동안 머신 slot은 점유**한다. 정리 실패 시 `Dying` 유지: 머신 healthy면 tick(30s)마다 재시도, unhealthy면 복귀 후 전체 동기화에서 정리. [§8.3]
- 입양된 unit은 `ps -a` 결과만으로 진입한다: 살아 있으면 `Starting`(CreatedAt = 컨테이너 생성 시각, 등록 여부는 보지 않음), `exited`면 `Dying`. 다음 tick이 `GetRunner`로 `Running` 승격 또는 grace 초과 `Dying`을 판정한다. Reconcile은 GitHub을 조회하지 않는다(§5 책임 경계). [§8.3]

집계 정의 [§7.2-3, §8.3]:
- `running(scaleSet)` = Creating + Starting + Running + Draining (desired 계산용)
- `occupied(machine)` = running + Dying(부품 잔존) + Foreign unit (spread·여유 슬롯 계산용)

**정리 순서** (Dying, 기동 타임아웃, 재시작 후 exited 발견, startUnit 실패 역순 정리 모두 동일): [§8.3]
```
GetRunner(RunnerName) → (found) RemoveRunner → runner 컨테이너 rm → sidecar rm → 볼륨 3개 rm → slice stop + revert
```

### 3.3 Machine health  [§7.1-3, §7.1-8, §10.1]

- `Healthy` → `Unhealthy`: SSH 단절, events 스트림 종료, pre-pull 실패, `info` 실패. capacity에서 제외.
- `Unhealthy` → `Healthy`: 백오프 재접속 성공 + 전체 동기화 완료.
- `Failed`: preflight의 설정·환경 모순(R16, R21). 시작 시 발견되면 프로세스 시작 실패. 재접속 후 preflight에서 발견되면 그 머신만 `Failed`로 두고 오류 로그. `Failed`는 capacity와 배치에서 제외되고 **재접속 대상에서도 제외**된다(에이전트 goroutine 종료). [§7.1-3, R21]

### 3.4 resource (Phase 1)  [§6.0, §8.1, §9.3, R25]

```go
package resource

type Budget struct {
    CPU         float64   // 코어 수. 0.5 허용. > 0
    MemoryBytes int64     // 바이트. > 0
}

func ParseCPU(v any) (float64, error)        // YAML 숫자(int/float) 또는 문자열 "2", "0.5". 음수·0·비숫자 → 오류 [R25]
func ParseMemory(s string) (int64, error)    // "512Mi", "4Gi", "1024Ki" (1024 진법). 접미 없음·다른 단위 → 오류 [R25]
func ParseBudget(cpu any, memory string) (Budget, error)

func (b Budget) DockerFlags() []string       // ["--cpus=2", "--memory=4294967296"] [§9.3 표]
func (b Budget) SystemdProps() (cpuQuota, memoryMax string)   // "200%", "4G". cpuQuota 는 cpu×100 을 그대로 쓴 문자열(0.125 → "12.5%"). memoryMax 는 바이트를 1024로 나누어 떨어지는 가장 큰 단위(K/M/G)로 표기 [§9.3 표]
```

`domain`은 `resource.Budget`을 `ScaleSet.Unit`에 쓴다. 머신 예산은 preflight(§7.1)가 config 또는 `runtime.Info`로 조립해 `PhysicalMax`/`EffectiveMax`를 계산하고, `domain.Machine`에는 그 결과인 정수 두 개만 남는다(재접속 시 preflight를 다시 돌고, `plan.Capacity`는 `EffectiveMax`만 읽는다). `config`는 파싱에 `resource.Parse*`를 호출한다.

## 4. 인터페이스

### 4.1 Executor  [§5, §10]

```go
package executor

type Cmd struct {
    Argv  []string
    Stdin io.Reader        // nil 이면 없음. docker cp - 의 tar 스트림 용도
    Sudo  bool             // true 면 "sudo -n" 접두 [§10.2 판단 규칙]
}

type Result struct {
    Stdout, Stderr []byte
    ExitCode       int
}

type Executor interface {
    Run(ctx context.Context, c Cmd) (Result, error)
    // 장기 스트림. 반환 ReadCloser 가 닫히면(원격 종료·단절) 호출자가 백오프로 재시작한다.
    Stream(ctx context.Context, c Cmd) (io.ReadCloser, error)
    Close() error
}

func NewLocal() Executor
func NewSSH(cfg SSHConfig) (Executor, error)   // 연결 유지, host key 검증(fingerprint → known_hosts → 거부)
```

`SSHConfig`: Host, Port, User, KeyFile, KeyPassphrase, Fingerprint, KnownHostsFile, InsecureSkipHostKeyVerify, ConnectTimeout(10s 상수). 구현 라이브러리: `golang.org/x/crypto/ssh` + `knownhosts`.

### 4.2 Runtime  [§5, §7, §9]

```go
package runtime

type Info struct {
    CPUs          float64
    MemoryBytes   int64
    Rootless      bool          // podman 만 의미
    CgroupDriver  string        // "systemd" 기대
    CgroupVersion string        // "2" 기대. podman 의 "v2" 는 "2" 로 정규화
}

type Container struct {
    Name    string              // gh-ars-<unit>-runner|sidecar
    Labels  map[string]string
    State   string              // running | exited | created ...
    Created time.Time
}

type Event struct {
    Name     string             // 컨테이너 이름 → unit/role 파싱 [§4.2]
    Action   string             // "die" | "start" | ...
    ExitCode int
    At       time.Time
}

type CreateSpec struct {
    Name, Image      string
    Labels           map[string]string
    Entrypoint       []string
    Cmd              []string
    Env              map[string]string   // DOCKER_HOST 등. JIT 값은 절대 넣지 않음
    Volumes          []Mount             // name → containerPath
    CgroupParent     string              // sidecar 모드: gh-ars-<unit>.slice
    CPUs             float64             // none 모드만 (0 이면 미지정)
    MemoryBytes      int64               // none 모드만
    Privileged       bool                // sidecar 컨테이너만
}

type Runtime interface {
    Kind() domain.RuntimeKind
    Info(ctx) (Info, error)
    Pull(ctx, image string) error
    List(ctx, labelFilter string) ([]Container, error)           // 항상 ps -a
    Create(ctx, spec CreateSpec) error
    CopyIn(ctx, container string, tar io.Reader, destDir string) error   // docker cp - <ctr>:<destDir>
    Start(ctx, container string) error
    Remove(ctx, container string, force bool) error
    VolumeCreate(ctx, name string, labels map[string]string) error
    VolumeList(ctx, labelFilter string) ([]string, error)
    VolumeRemove(ctx, name string) error
    Events(ctx, labelFilter string) (<-chan Event, <-chan error)  // 스트림 종료 시 error 채널로 통지
}

func NewDocker(ex executor.Executor, sudo bool) Runtime   // sudo 는 §10.2 판단 규칙 결과 (docker 는 항상 false)
func NewPodman(ex executor.Executor, sudo bool) Runtime
```

docker/podman 구현은 argv 조립과 출력 파싱(`--format '{{json .}}'`)만 다르다. 공통 골격은 `cli.go`, 차이는 `docker.go` / `podman.go`.

**sidecar 컨테이너 CreateSpec 값** [§9.1]:

| 필드 | docker 머신 | podman 머신 |
|---|---|---|
| Name | `gh-ars-<unit>-sidecar` | 동일 |
| Image | `jobRuntime.image` 또는 `docker:29.7.2-dind` | `jobRuntime.image` 또는 `quay.io/podman/stable:v5.8.4` |
| Entrypoint/Cmd | `dockerd --host=unix:///var/run/docker.sock` | `podman system service --time=0 unix:///var/run/docker.sock` |
| Env | `DOCKER_TLS_CERTDIR=""` | – |
| Privileged | true | true |
| CgroupParent | `gh-ars-<unit>.slice` | 동일 |
| Volumes | sock→`/var/run`, work→`/home/runner/_work`, externals→`/home/runner/externals` | 동일 |
| Labels | unit 라벨 세트, role=sidecar | 동일 |

runner 컨테이너(sidecar 모드)는 같은 볼륨 3개, `CgroupParent` 동일, Env에 `DOCKER_HOST=unix:///var/run/docker.sock`(podman 머신이면 `CONTAINER_HOST` 추가), Privileged=false, CPUs/MemoryBytes 미지정.

**JIT tar** (`internal/runtime/jittar.go`) [§7.2-4, TESTPLAN §1]: 엔트리 1개 `.jitconfig`, mode 0600, uid/gid 1001, 내용 = EncodedJITConfig + `\n`. `CopyIn(ctx, runnerCtr, tar, "/home/runner")`. 유닛 테스트 대상.

**entrypoint 래퍼** (`internal/runtime/wrapper.go`) [§7.2-4]: `Wrapper(mode domain.Mode) []string`이 `Entrypoint=["/bin/bash"]`, `Cmd=["-c", <script>]`를 돌려준다. script는 SPEC §7.2-4의 none/sidecar 문자열을 그대로 상수로 둔다(sidecar는 소켓 대기 150×0.2s 선행). 유닛 테스트는 두 상수가 SPEC 문자열과 일치하는지 검사한다.

### 4.3 systemd  [§9.3, §10.2]

```go
package systemd

type Slices interface {
    Check(ctx) error                                                          // systemctl --version (root 아니면 sudo -n). preflight R16
    Create(ctx, name string, cpuQuota, memoryMax string) error             // set-property --runtime. 인자는 resource.Budget.SystemdProps() 반환값 그대로 (§3.4)
    Remove(ctx, name string) error                                           // stop + revert
    List(ctx, prefix string) ([]string, error)                               // 고아 slice 탐색
}
func New(ex executor.Executor, sudo bool) Slices   // sudo = !root (§10.2 규칙 4)
```

### 4.4 GitHub  [§7.1-5, §7.2, §8.3, §11]

actions/scaleset v0.4.0의 실제 시그니처에 맞춘 얇은 래퍼.

```go
package github

type RunnerRef struct{ ID int64; Name string }

type Client interface {
    // GetRunnerGroupByName → GetRunnerScaleSet(groupID, name) → 없으면 CreateRunnerScaleSet. 그룹 이동 없음 [§7.1-5 각주]
    EnsureScaleSet(ctx, name, runnerGroup string) (id int, err error)
    // 같은 조회 경로 → DeleteRunnerScaleSet(id) [§11]
    DeleteScaleSet(ctx, name, runnerGroup string) error
    // RunnerScaleSetJitRunnerSetting{Name: runnerName, WorkFolder: "/home/runner/_work"} → EncodedJITConfig, Runner
    GenerateJIT(ctx, scaleSetID int, runnerName string) (encoded string, runner RunnerRef, err error)
    // GetRunnerByName. 라이브러리가 미존재를 (nil, nil) 로 돌려주므로 found=false 로 변환
    GetRunner(ctx, runnerName string) (ref RunnerRef, found bool, err error)
    // errors.Is(err, scaleset.RunnerNotFoundError) → nil (ephemeral runner 가 스스로 해제한 경우) [§8.3]
    // 라이브러리의 newRequestResponseError 가 응답 본문의 AgentNotFoundException 을 이 sentinel 로 감싼다
    // 축소(§7.2-3)에서는 거절을 busy 로 해석한다: IsBusy(err) 참조
    RemoveRunner(ctx, runnerID int64) error
    // *scaleset.MessageSessionClient 반환. GetMessage/DeleteMessage/AcquireJobs/Session 을 갖춰 listener.Client 를 만족한다
    NewSession(ctx, scaleSetID int, owner string) (listener.Client, error)
}

// IsBusy: RemoveRunner 오류가 "job 진행 중"을 뜻하면 true.
// errors.Is(err, scaleset.JobStillRunningError) 이거나 응답 상태가 4xx(404 제외)인 경우. [§7.2-3]
func IsBusy(err error) bool

func New(cfg config.GitHub, log *slog.Logger) (Client, error)
// PAT → scaleset.NewClientWithPersonalAccessToken, App → scaleset.NewClientWithGitHubApp (InstallationID int64)
```

R7의 `Default` 비교는 대소문자 무시(`strings.EqualFold`). [§6.2]

### 4.5 Controller ↔ listener  [§7.2]

scale set마다 `listener.Listener` 하나를 `Run(ctx, scaler)`로 띄운다. `Controller`가 scale set별 `listener.Scaler`를 구현한다.

```go
func (s *scaleSetScaler) HandleDesiredRunnerCount(ctx, count int) (int, error)
    // count = TotalAssignedJobs. Controller 루프에 msgDesired 를 보내고 결과(실제 running 목표)를 기다린다.
func (s *scaleSetScaler) HandleJobStarted(ctx, *scaleset.JobStarted) error   // RunnerName 으로 unit 을 찾아 msgJobStarted → Busy=true [§7.2-3]
func (s *scaleSetScaler) HandleJobCompleted(ctx, *scaleset.JobCompleted) error // 로그만 (ephemeral 이라 die 가 뒤따른다)
```

확인된 listener 동작(v0.4.0 소스 기준):
- `JobAvailableMessages`를 **전부** `AcquireJobs`한다. [§7.2-2]
- 초기 세션의 `Statistics.TotalAssignedJobs`와 이후 **매 메시지**의 `TotalAssignedJobs`를 `HandleDesiredRunnerCount`에 넘긴다. 반환값은 메트릭 기록에만 쓴다.
- `SetMaxRunners(n)`은 atomic 저장이며 다음 `GetMessage`의 `maxCapacity`에 반영된다. [§7.2-1]
- `listener.Config.Validate`는 `0 ≤ MaxRunners ≤ MaxInt32`를 요구한다. **MaxRunners 0이 허용**되므로 capacity 0인 scale set도 listener를 정상 시작하고, 운영 중 `SetMaxRunners(0)`도 허용된다. 중지·재시작 로직은 필요 없다.
- 초기 세션의 `Statistics`가 nil이면 `Run`이 오류를 반환한다. Controller는 listener 오류를 로그 후 백오프(§7 백오프와 동일 수열)로 재시작한다.

## 5. 순수 로직 (`internal/plan`)  [§8]

입력 타입은 전부 `domain` 패키지 것이다(`plan → domain → resource`). `plan`은 자체 입력 구조체를 두지 않는다. 예외는 `Observed`/`Action`/`ReconcileConfig`로, 이 셋만 `plan`이 정의한다.

```go
package plan

func PhysicalMax(machine, unit resource.Budget) int                                   // floor(min(cpu/cpu, mem/mem)) [§8.1]
func EffectiveMax(physical int, maxRunners *int) int                                  // [§8.1, R22]
func Capacity(ss domain.ScaleSet, machines []domain.Machine) int                      // Healthy 만 합산, ss.MaxRunners 로 cap [§8.1, R23]
func Desired(capacity, minRunners, assigned int) int                                  // min(cap, max(min, assigned)) [§7.2-3]
func Spread(machines []domain.Machine, occupied map[string]int, now time.Time) (string, bool) // 사용률 최저 → 여유 슬롯 → LastPlacedAt → 순서 [§8.2]
func ScaleDown(units []domain.Unit, remove int) []domain.UnitID                       // Running && !Busy, CreatedAt 오래된 순, remove 개 [§7.2-3]
func Reconcile(obs Observed, known map[domain.UnitID]domain.Unit, now time.Time, cfg ReconcileConfig) []Action  // [§8.3]

type Observed struct {                 // 한 머신의 재동기화 스냅샷. GitHub 정보는 없다
    Machine    string
    Containers []runtime.Container     // ps -a 결과 (runner/sidecar)
    Volumes    []string
    Slices     []string
}
type ReconcileConfig struct{ KnownScaleSets map[string]domain.Mode }   // Foreign 판정용
type Action struct {
    Kind ActionKind                    // Adopt | RemoveUnit | RemoveOrphan
    Unit domain.Unit                   // Adopt, RemoveUnit
    Orphan struct{ Kind, Name string } // RemoveOrphan: container | volume | slice
}
```

**책임 경계 (tick vs Reconcile)**: GitHub 등록 대조는 `msgTick`에서만 한다. `Reconcile`은 재동기화 시점의 **부품 집합만** 판정한다. 그래서 `Observed`에 등록 여부가 없고, SPEC §8.3의 "runner 살아 있는데 등록 없음" 행은 `Reconcile`이 아니라 Controller의 tick 처리가 구현 주체다. 입양된 살아 있는 unit은 등록 여부를 보지 않고 `Starting`으로 들어가며(CreatedAt = 컨테이너 생성 시각), 다음 tick이 `GetRunner`로 `Running` 승격 또는 grace 초과 `Dying`을 판정한다(§3.2). `runtime.Container`를 입력으로 받기 위해 `plan → runtime`의 타입 의존이 생기지만 `runtime`의 I/O 함수는 호출하지 않는다.

`occupied` 계산에서 `Foreign` unit은 머신 slot 1개, `Dying` unit은 부품이 남아 있는 동안 slot 1개로 센다. [§8.3]

GitHub 등록 제거는 별도 Action이 아니라 **`RemoveUnit` 실행의 첫 단계**(§3.2 정리 순서)다. SPEC §8.3 표의 각 행이 테스트 케이스 하나이며, 등록 관련 두 행(등록 없음, 정리 시 등록 처리)은 `controller` 테스트에 속한다(TESTPLAN §1 태그 참조).

## 6. Controller 루프  [§7]

```go
type Controller struct {
    cfg      *config.Config
    gh       github.Client
    machines map[string]*machine.Agent      // 이름 → 에이전트
    state    state                          // units, machines health, listeners
    inbox    chan message                   // 아래 메시지 타입
}

// inbox 메시지
type (
    msgDesired      struct{ ScaleSet string; Assigned int; Reply chan int }   // listener 콜백
    msgEvent        struct{ Machine string; Ev runtime.Event }
    msgHealth       struct{ Machine string; Healthy bool }
    msgResynced     struct{ Machine string; Obs plan.Observed }
    msgTick         struct{}                                   // 30s 코드 상수 [§8.3 상수 표]
    msgUnitStarted  struct{ Unit UnitID; Err error }           // 비동기 create→cp→start 완료
    msgJobStarted   struct{ ScaleSet, RunnerName string }      // listener HandleJobStarted → Busy=true
    msgDrainResult  struct{ Unit UnitID; Busy bool; Err error } // 비동기 RemoveRunner(축소) 결과
    msgCleanupDone  struct{ Unit UnitID; Err error }           // 비동기 cleanupUnit 결과
)
```

루프는 `select` 하나로 메시지를 처리한다. 메시지별 후속 동작:

| 메시지 | 동작 |
|---|---|
| `msgDesired` | capacity 재계산 → 바뀌면 `SetMaxRunners`. `desired = Desired(capacity, minRunners, Assigned)`. `create = desired − running > 0`이면 **신규 unit 생성은 여기서만**: spread(occupied 기준)로 머신 선택 → `Creating` 등록 → goroutine `startUnit`. `remove = running − desired > 0`이면 **축소도 여기서만**: `plan.ScaleDown`으로 후보 선정 → `Draining` → goroutine으로 `RemoveRunner`(결과는 `msgDrainResult`). Reply에 running 목표를 보낸다. [§7.2-3] |
| `msgDrainResult` | 성공: `Draining` 유지, `die`를 기다린다. `Busy`(IsBusy) 또는 기타 오류: `Running`으로 복귀(오류는 로그). unit이 이미 `Dying`이면 무시. [§7.2-3] |
| `msgJobStarted` | RunnerName으로 unit을 찾아 `Busy=true`. 못 찾으면 로그만. |
| `msgEvent` (`die`, role=runner) | unit(Creating/Starting/Running/Draining 모두)을 `Dying`으로 → goroutine `cleanupUnit`. 그 뒤 `minRunners` 미달분만 보충하고 **assigned 기반 신규 생성은 하지 않는다**. [§7.2-6] |
| `msgEvent` (`die`, role=sidecar) | runner가 살아 있으면 로그만(runner die 시 함께 정리). |
| `msgHealth` | 머신 health 갱신 → capacity 재계산 → `SetMaxRunners`. unhealthy 머신의 Dying unit은 정리를 보류한다. |
| `msgResynced` | `plan.Reconcile` 실행 → Adopt/RemoveUnit/RemoveOrphan 적용(보류된 Dying 포함). 부품 집합만 판정하고 GitHub은 조회하지 않는다. 머신 healthy 복귀. |
| `msgTick` | **GitHub 등록 대조의 유일한 구현 주체.** Starting/Running unit마다 `GetRunner`로 대조(**Draining 제외**): Starting은 등록되면 `Running`, grace 초과면 `Dying`; Running은 미등록이면 `Dying`. `Creating`의 기동 타임아웃 판정. healthy 머신의 `Dying` unit 중 정리가 진행 중이 아닌 것은 `cleanupUnit` 재시도. [§8.3] |
| `msgUnitStarted` | 성공: `Creating → Starting`. 실패: `Dying`으로 두고 `cleanupUnit`(역순 정리, RemoveRunner 포함). 다음 `msgDesired`에서 재배치. unit이 이미 `Dying`이면 무시. |
| `msgCleanupDone` | 성공: `Removed`(상태에서 삭제, slot 해제). 실패: `Dying` 유지, 다음 tick에서 재시도. |

`startUnit` (goroutine, 상태를 직접 만지지 않는다) [§7.2-4]:
```
JIT 생성(GitHub) → [sidecar: slice Create, 볼륨 3개 Create, sidecar 컨테이너 Create+Start]
→ runner 컨테이너 Create(entrypoint 래퍼, 라벨, 리소스) → CopyIn(jittar) → Start
```
전체에 2분 타임아웃 컨텍스트. 실패 시 §3.2 정리 순서(GetRunner → RemoveRunner 포함)로 만든 부품을 정리하고 `msgUnitStarted{Err}`.

`cleanupUnit` (goroutine): §3.2 정리 순서 실행 → `msgCleanupDone{Err}`. 실패해도 부분 정리된 부품은 다음 시도에서 건너뛴다(각 단계는 "없으면 성공"으로 멱등).

## 7. Machine 에이전트 (`internal/machine`)  [§7.1, §10]

머신마다 goroutine 하나. 책임: 접속 유지, preflight, pre-pull, `Events` 스트림 수신 → `msgEvent`, 단절 시 백오프 재접속 → 재접속 후 `Observe()`(ps -a, 볼륨, slice) → `msgResynced`. Controller는 `Agent.Runtime()`, `Agent.Slices()`로 명령을 보낸다.

preflight 순서 (SPEC §10.2 판단 규칙과 §7.1-3):
1. SSH 머신: host key 검증 후 접속(타임아웃 10s). local: 생략.
2. `id -u` → root 여부.
3. runtime `info`:
   - docker: `docker info`(sudo 없음). 실패 → none은 unhealthy, sidecar는 R16 오류.
   - podman: `podman info` → 실패면 `sudo -n podman info`(모드 무관). 성공한 경로를 `Machine.Sudo`로 고정. 둘 다 실패 → none은 unhealthy, sidecar는 R16 오류. sidecar이고 고정 경로의 `Rootless=true`면 아직 시도하지 않은 `sudo -n podman info`를 시도해 rootful이면 `Sudo=true`로 바꾸고, 그래도 rootless면 R16 오류. [§10.2 규칙 3]
4. sidecar scale set이면: `CgroupDriver == systemd && CgroupVersion == 2` 확인, `Slices.Check()`(root 아니면 sudo -n) → 실패 시 R16 오류.
5. resources: 생략 시 `info`의 CPUs/MemoryBytes, 명시 시 탐지값으로 cap(R21 경고). physicalMax·effectiveMax 계산(R21 오류, R22 경고).
6. pre-pull: runner 이미지, sidecar면 sidecar 이미지. 실패 → unhealthy.

시작 시와 재접속 시의 차이: 시작 시 R16/R21 오류는 프로세스 시작 실패. 재접속 후 preflight에서 같은 오류가 나면 `msgHealth{Failed}`를 보내고 에이전트 goroutine을 종료한다(재접속 없음). R24는 Controller가 시작 시 한 번 평가한다: 전 머신 도달이면 위반 시 시작 실패, 미도달 머신이 있으면 경고. [§6.2 R21, R24]

`NewDocker/NewPodman(ex, sudo)`와 `systemd.New(ex, sudo)`의 `sudo`는 위 2·3·4단계 결과에서 나온다(docker: 항상 false, podman: 3단계 결과, systemd: `!root`).

백오프: 1s 시작, ×2, 최대 30s, ±20% jitter, 성공 시 리셋. local 머신은 events 재시작에만 적용. [§7.1-8]

## 8. 설정 로딩 (`internal/config`)  [§6]

- 라이브러리: `gopkg.in/yaml.v3` + `Decoder.KnownFields(true)` (R2).
- 파이프라인: `파일 읽기 → strict 디코드(KnownFields) → secret 필드가 ${env:}/${file:} 참조 형식인지 struct에서 검사(R4) → 필드 값 치환(R5) → 기본값 적용 → 정적 검증(R2~R15, R17~R20, R23 필수성, R25)`. 원문 텍스트 치환은 하지 않는다(여러 줄 PEM이 YAML을 깨뜨림).
- 동적 규칙(R16, R21, R22, R23 cap, R24)은 preflight 이후 `plan`에서 판정한다.
- `github.auth.app.installationId`는 `int64`.
- 검증 오류는 전부 모아 한 번에 보고한다(`errors.Join`).

## 9. CLI (`cmd/gh-ars`)  [§11]

```
gh-ars run -c <file> [--log-level ...] [--log-format ...]
gh-ars scaleset delete <name> -c <file>
```
`run`: config → logging → github.New → Controller.Run(ctx). SIGINT/SIGTERM으로 ctx 취소 → listener 종료, 에이전트 종료, 컨테이너는 그대로 둔다. [§7.3]
`scaleset delete`: config에서 해당 scale set의 `runnerGroup`(없으면 `Default`) → `DeleteScaleSet(name, runnerGroup)`.

## 10. 외부 의존성

go.mod에는 **처음 쓰는 Phase에서** 그 의존성만 추가한다(`go get <pkg>@<version>` 후 그 Phase 커밋에 go.mod·go.sum을 포함). 쓰지 않는 의존성을 미리 올려두지 않는다: 왜 필요한지가 코드와 같은 커밋에 드러나고, 결국 안 쓰게 된 패키지가 go.mod에 남지 않는다. 버전은 추가하는 시점에 고정하며, 아래 표에 없는 패키지를 넣으려면 먼저 보고하고 이 표에 행을 추가한다.

| 용도 | 패키지 | 도입 Phase |
|---|---|---|
| Scale Set API | `github.com/actions/scaleset` **v0.4.0**(2026-09-06 기준 최신 태그. go.mod에 고정하고 갱신은 명시적 결정으로만), `.../listener` | 6 (`github`) |
| SSH | `golang.org/x/crypto/ssh`, `golang.org/x/crypto/ssh/knownhosts` | 8 (`executor/ssh`) |
| YAML | `gopkg.in/yaml.v3` | 2 (`config`) |
| ULID | `github.com/oklog/ulid/v2` | 7 (`controller` — unit id 발급. Phase 1의 `domain`은 형식 검증만 해서 필요 없다) |
| 로그 | 표준 `log/slog` | – (표준 라이브러리) |
| tar | 표준 `archive/tar` | – (표준 라이브러리) |

## 11. 테스트 전략  [TESTPLAN.md]

- `plan`, `resource`, `config`, `domain`: 테이블 기반 유닛 테스트. SPEC 규칙 번호를 테스트 이름에 넣는다(`TestValidate_R15_SidecarRuntimeMismatch`).
- `runtime`: Executor를 fake로 바꿔 argv 조립과 출력 파싱을 검증한다(실제 docker 불필요). `jittar.go`는 tar 엔트리(경로·mode·uid/gid·내용)를 검증.
- `controller`: fake GitHub·fake Agent로 메시지 시퀀스를 넣고 상태 전이와 호출 순서를 검증한다. 필수 케이스: die 후 신규 생성 없음, Creating 중 die, startUnit 실패 역순 정리(RemoveRunner 포함), tick의 Starting/Running 대조(Draining 제외), 축소 시 후보 선정과 `msgDrainResult` busy 복귀, Draining 중 die → Dying, cleanup 실패 후 tick 재시도와 slot 점유 유지, 재접속 후 R21 → Failed.
- `plan.ScaleDown`: Running·non-busy·오래된 순, remove 개 초과 선택 금지, Creating/Starting/Draining 제외.
- `executor/ssh`: host key 검증 순서만 유닛 테스트(fingerprint → known_hosts → 거부). 실제 접속은 E2E.
- E2E는 `docs/TESTPLAN.md` §2 수동 체크리스트(미확정 동작 2개의 대안 포함). 결과는 `PLAN.md`에 기록.

## 12. 구현 순서

`PLAN.md`의 "단계별 완료 기준" 표로 옮겼다.

## 13. 설계 결정 기록

`docs/DECISIONS.md`로 분리했다.
