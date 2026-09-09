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

의존 방향: `cmd → {controller, config, github, logging}`, `controller → {github, machine, plan, domain, config, resource, runtime(타입만)}`, `config → {domain, resource}`, `plan → {domain, resource, runtime(타입만)}`, `domain → resource`, `machine → {runtime, systemd, executor, domain, resource}`, `runtime → {executor, domain}`. `plan`, `domain`, `resource`는 I/O를 수행하지 않는다(`plan`이 `runtime.Container` 타입을 참조하는 것은 허용, 함수 호출은 금지).

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
    Completed  bool            // JobCompleted 수신(die 보다 먼저 온 경우). die 시 pendingCompletion 에 넣지 않는다 [§7.2-3]
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
- `occupied(machine)` = running + Dying(부품 잔존) + Foreign unit (spread의 사용률·여유 슬롯 계산용. §8.2)

**정리 순서** (Dying, 기동 타임아웃, 재시작 후 exited 발견, startUnit 실패 역순 정리 모두 동일): [§8.3]
```
GetRunner(RunnerName) → (found) RemoveRunner → runner 컨테이너 rm → sidecar rm → 볼륨 3개 rm → slice stop + revert
```

### 3.3 Machine health  [§7.1-3, §7.1-8, §10.1]

- `Healthy` → `Unhealthy`: SSH 단절(keepalive 연속 실패 포함, §10.1), events 스트림 종료, pre-pull 실패, `info` 실패. capacity에서 제외. **명령 하나의 실패·타임아웃은 트리거가 아니다** — 그것은 그 unit의 정리·기동 재시도(§8.3)로 처리한다.
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
    // 프로세스가 실행되어 종료 코드를 남겼으면 오류가 아니라 Result.ExitCode 로 돌려준다.
    // 오류는 프로세스를 시작하지 못한 경우와 ctx 취소뿐이다.
    Run(ctx context.Context, c Cmd) (Result, error)
    // 장기 스트림. 반환 ReadCloser 가 닫히면(원격 종료·단절) 호출자가 백오프로 재시작한다.
    // 정상 종료는 io.EOF, 비정상 종료는 argv 와 stderr 를 담은 오류로 마지막 Read 가 알린다.
    // 호출자는 다 쓰면 Close 한다. 프로세스 회수는 ctx 취소만으로도 되지만, Close 는
    // ctx 와 무관하게 스트림을 끝내고 파이프·goroutine 정리를 기다리는 유일한 방법이다.
    Stream(ctx context.Context, c Cmd) (io.ReadCloser, error)
    Close() error
}

func NewLocal() Executor
func NewSSH(cfg SSHConfig) (Executor, error)   // 연결 유지, host key 검증(fingerprint → known_hosts → 거부)
```

**비0 종료는 오류가 아니라 값이다.** 종료 코드로 분기하는 호출자가 있기 때문이다: preflight 의 podman 경로 고정(§10.2 규칙 3)은 `podman info` 실패를 보고 `sudo -n podman info` 로 넘어가고, 정리 단계(§8.3)는 "이미 없음"을 성공으로 취급한다. 오류를 받아야 하는 실패(바이너리 없음, 접속 끊김, 취소)와 명령의 정상적인 부정 응답을 호출자가 매번 풀어보지 않고 구분하게 한다.

**파이프 대기에는 상한이 있다.** 파이프는 자식의 fd 를 물려받은 손자가 살아 있는 동안 열려 있어, 자식이 죽어도 EOF 가 오지 않을 수 있다(`sudo -n podman info` 가 이 모양이다). 상한이 없으면 `Run` 과 스트림의 `Read`·`Close` 가 손자의 수명만큼 매달려, 기동 타임아웃 2분(§7.2-4)이 지나도 `startUnit` goroutine 이 풀리지 않고 events 재시작(§7.1-8)도 영영 일어나지 않는다. 그래서 자식이 끝난 뒤 파이프가 닫히기를 기다리는 시간을 상한(코드 상수 `pipeDrainDelay`)으로 막는다. 이것은 관측 가능한 계약을 바꾸지 않는 내부 방어 상수라 SPEC §8.3 표가 아니라 여기에 둔다(§8.3 머리말 기준).

이 상한이 스트림에서도 실제로 걸리려면 `Wait` 이 `Read` 와 나란히 돌아야 한다. `StdoutPipe` 는 파이프를 닫는 주체가 `Wait` 이라 "다 읽은 뒤 `Wait`" 순서를 요구하는데, 손자가 stdout 을 물고 있으면 `Read` 가 끝나지 않아 `Wait` 이 시작되지도 못하고 상한이 발동할 기회가 없다. 그래서 스트림은 `io.Pipe` 를 `cmd.Stdout` 으로 주고 `Wait` 을 곧바로 돌린 뒤, 그 결과를 파이프에 실어 `Read` 의 종료 통지로 만든다.

**stdin 복사도 `exec` 에 맡기지 않는다.** `Cmd.Stdin` 을 그대로 넘기면 `exec` 가 복사 goroutine 을 세우고 `Wait` 이 그것을 기다리는데, 그 goroutine 이 `Read` 에 막혀 있으면 `WaitDelay` 가 목적지 파이프를 닫아도 풀리지 않는다. 상한이 stdin 경로에서만 조용히 무효가 되는 것이다. `*os.File` 을 주면 `exec` 는 fd 를 그대로 넘기고 복사하지 않으므로, 막히는 쪽은 우리 goroutine 뿐이고 `Run` 과 `Close` 는 상한을 지킨다. 대신 호출자는 **즉시 반환하는 Reader** 를 준다 — §7.2-4 의 tar 스트림은 메모리에서 만들므로 이 조건을 만족한다.

이 상한은 ctx 취소뿐 아니라 **자식의 정상 종료에서도 시작한다**(`os/exec` 의 `WaitDelay` 계약). 손자가 없으면 자식 종료와 함께 파이프가 닫혀 발동하지 않지만, 발동했다면 출력이 잘렸을 수 있다. 그때 종료 코드 0을 성공 값으로 돌려주면 `docker ps` 결과가 조용히 잘려 §8.3 판정이 틀어지므로, `Run` 은 그것을 값이 아니라 오류로 알린다.

**예외 하나**: 자식이 비0으로 끝나면 `os/exec` 가 `ExitError` 를 우선해 드레인 만료가 가려지고, 잘렸을 수 있는 출력이 `Result` 로 나간다. 비0 응답에서 stdout 을 신뢰하는 호출자가 없어(§10.2 규칙 3은 성패만, §8.3은 "이미 없음"만 본다) 허용한다.

`SSHConfig`: Host, Port, User, KeyFile, KeyPassphrase, Fingerprint, KnownHostsFile, InsecureSkipHostKeyVerify, Log(R20 경고용). 접속 타임아웃 10s는 설정이 아니라 코드 상수 `sshConnectTimeout`이다(§3.2: 상수의 설정 노출은 non-goal). 구현 라이브러리: `golang.org/x/crypto/ssh` + `knownhosts`.

**ctx 취소는 채널만 끝낸다.** 위 계약이 ctx 취소를 명령 단위의 정상적인 결과로 규정하므로(local은 프로세스만 죽인다) ssh 구현도 그 채널만 닫고 접속은 유지한다. 접속을 닫는 것은 keepalive 연속 실패(연결 수준 신호)와 명시적 `Close`뿐이다. 상한 안에 회수되지 않은 복사 goroutine은 **버린다**: 접속을 닫아 강제 회수하면 그 머신의 events 스트림과 동시 실행 중인 다른 명령까지 끊기고, 버려진 goroutine은 원격이 응답하거나 접속이 끝날 때(그 접속이 정말 죽었다면 keepalive가 걷어낸다) 사라진다. 버린 뒤에는 **그들이 쓰는 버퍼를 읽지 않는다** — `Run`의 stdout/stderr는 잠금 없는 `bytes.Buffer`라, 회수를 확인한 경우에만 오류 메시지에 stderr를 싣는다(`Stream`이 쓰는 `capBuffer`는 잠금이 있어 제약이 없다). [SPEC §10.1, §8.3]

**keepalive.** 접속마다 `keepalive@openssh.com` 요청을 주기적으로 보내고, 연속 실패가 허용 미스(SPEC §8.3)에 닿으면 접속을 닫아 §10.1의 "단절 → unhealthy → 재접속" 경로로 보낸다. half-open 접속은 events가 조용하고 명령도 매달리기만 해서 다른 신호가 없다.

프로브는 **응답을 동기로 기다리지 않는다**: `SendRequest(wantReply=true)`는 응답이 오거나 접속이 죽을 때까지 막히는데, half-open에서 그 시점은 TCP 스택이 포기할 때(Linux 기본 ≈15분)라 §8.3이 약속한 `주기 × 미스` 상한이 무너진다. 프로브는 goroutine으로 띄우고 **다음 주기까지 응답이 없으면 그 자체를 miss로 센다**(OpenSSH의 `ServerAliveInterval`/`ServerAliveCountMax`와 같은 의미론). 프로브는 한 번에 하나만 띄우고, 주기 직전에 도착한 결과는 먼저 반영한다(응답한 프로브를 "응답 없음"으로 세지 않도록). 판정부는 시간도 전송도 모르는 상태 기계(`keepaliveState`)로 분리해 표로 검증한다.

**host key 알고리즘 선호 순서를 직접 정한다.** x/crypto의 기본 순서는 ed25519를 마지막에 두어 서버가 ecdsa/rsa를 고르게 만드는데, 사용자가 기록해 둔 `known_hosts` 항목·`fingerprint`는 보통 OpenSSH가 협상한 ed25519다. 그대로 두면 `ssh user@host`가 되는 정상 호스트가 gh-ars에서만 host key 불일치로 거부된다(실측 2026-09-09). 그래서 **라이브러리의 지원 목록(`ssh.SupportedAlgorithms().HostKeys`)을 OpenSSH 순서(ed25519 → ecdsa → rsa)로 재정렬해** `ClientConfig.HostKeyAlgorithms`에 넣는다. 목록을 손으로 쓰지 않는 이유는 두 가지다: 인증서 알고리즘이 빠지면 `@cert-authority` 항목만 있는 호스트에 접속하지 못하고, 라이브러리가 보안 문제로 제외한 `ssh-rsa`(SHA-1)를 되살리게 된다.

재시도는 검증 방식마다 다르다. `known_hosts`는 그 호스트에 대해 다른 타입만 가진 경우 검증 실패에 `knownhosts.KeyError.Want`가 실려 오므로, 그 키들의 **계열**에 해당하는 지원 알고리즘으로 한 번 더 시도한다(파일의 와일드카드·해시 항목 매칭은 라이브러리가 이미 하므로 우리가 다시 파싱하지 않는다). `fingerprint`는 어떤 계열의 키를 기록해 둔 것인지 알 방법이 없으므로 **계열을 차례로**(ed25519 → ecdsa → rsa) 시도하고, 다 어긋나면 R20대로 거부한다 — 한 번만 재시도하면 지문이 3순위 키로 기록된 머신이 영구히 접속하지 못한다. 모든 시도는 같은 10s 예산을 공유한다. [§10.1, R20]

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

func NewDocker(ex executor.Executor, sudo bool, log *slog.Logger) Runtime   // sudo 는 §10.2 판단 규칙 결과 (docker 는 항상 false)
func NewPodman(ex executor.Executor, sudo bool, log *slog.Logger) Runtime
```

docker/podman 구현은 argv 조립과 출력 파싱만 다르다. 공통 골격은 `cli.go`, 차이는 `docker.go` / `podman.go`. `info`는 두 runtime 모두 `--format '{{json .}}'`를 파싱한다. `events`는 docker만 `--format '{{json .}}'`를 쓰고, podman은 `--format json`(SPEC §5 명시)이다 — 둘 다 결과는 JSON Lines 한 줄씩이지만 podman의 이벤트 스키마 자체가 docker와 다르다(`flavor.eventsFormat()`으로 분기). podman의 시각 필드는 버전에 따라 유닉스 정수(`time`, `timeNano`)로도 RFC3339 문자열(`Time`)로도 오므로 양쪽을 모두 받는다 — 정수를 `time.Time`으로 받으려다 unmarshal이 실패하면 그 줄이 통째로 버려지고, 그러면 **모든** 이벤트가 사라져 `die`가 영영 오지 않는다(실측 podman 6.1.1, 2026-09-09). 같은 이유로 `flavor.parseEvent`는 "관심 없는 줄"과 "읽지 못한 줄"을 구분한다. 구독 argv에 `--filter type=container`가 있으므로 다른 type이나 이름 없는 줄이 오는 것도 스키마 어긋남으로 센다. **첫 읽지 못한 줄은 즉시 `Warn`으로 남기고 스트림은 유지한다**(이후는 종료 오류에 싣는 집계로 갈음): 스키마가 통째로 어긋나면 스트림은 정상적으로 열린 채 며칠씩 유지되고 모든 줄이 조용히 버려지는데, 종료 오류만으로는 그 신호가 영영 나오지 않는다(§7.1-8의 재시작 트리거가 오지 않는다). 중간 통지를 `errCh`로 보내지는 않는다 — machine 층이 그것을 회차 종료로 읽는다. 그래서 `NewDocker`/`NewPodman`은 로거를 받는다(nil이면 `slog.Default()`). `ps`는 `{{json .}}`를 쓰지 않는다: 그 출력의 `Labels`는 "k=v,k=v"를 이스케이프 없이 이어붙인 문자열이라 이미지가 물려준 라벨 값에 `,gh-ars.mode=none` 같은 조각이 있으면 실제 라벨을 덮어쓸 수 있다. 대신 `{{.Names}}\t{{.State}}\t{{.CreatedAt}}\t{{.Label "gh-ars.unit"}}…` 처럼 §4.2의 gh-ars.* 키 5개를 하나씩 뽑는 탭 구분 템플릿을 쓰고, `Container.Labels`에는 그 키만 담는다(입양 복원에 그것만 필요하다). 비0 종료는 `*ExitError{Argv, ExitCode, Stderr}`로 올리고, `Remove`/`VolumeRemove`는 "이미 없음" 응답을 성공으로 흡수한다(§8.3 멱등).

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

**entrypoint 래퍼** (`internal/runtime/wrapper.go`) [§7.2-4]: `Wrapper(mode domain.Mode) (entrypoint, cmd []string, err error)`가 `Entrypoint=["/bin/bash"]`, `Cmd=["-c", <script>]`를 돌려주고, 알 수 없는 mode는 오류다. script는 SPEC §7.2-4의 none/sidecar 문자열을 그대로 상수로 둔다(sidecar는 소켓 대기 150×0.2s 선행). 유닛 테스트는 두 상수가 SPEC 문자열과 일치하는지 검사한다.

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
    // 시작 시 인증·scope 확인(§7.1-2). GetRunnerGroupByName 한 번으로 토큰 교환과 접근 권한을 확인한다.
    // New 는 네트워크를 타지 않으므로(라이브러리가 URL 파싱·HTTP 구성만 한다) 이 호출이 첫 접촉이다.
    // Controller 가 preflight·pre-pull 앞에서 부른다 — 없으면 잘못된 토큰이 §7.1-5 에서야 드러난다.
    // 라이브러리가 그룹 미존재도 오류로 돌려주므로(v0.4.0 count 0 → error) 잘못된 runnerGroup 도 여기서 걸린다
    // (§7.1-5 가 낼 오류를 앞당길 뿐이라 판정이 갈리지 않는다)
    CheckAuth(ctx, runnerGroup string) error
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
    // *scaleset.MessageSessionClient 반환. GetMessage/DeleteMessage/AcquireJobs/Session 을 갖춰 listener.Client 를 만족하고,
    // Close 로 세션을 지운다. listener.Client 에는 Close 가 없고 Listener.Run 도 세션을 닫지 않으므로(v0.4.0 listener.go)
    // Controller 가 세션을 보유하고 ctx 취소 시 Close 를 호출한다(SPEC §7.3). listener 재시작 시에도 이전 세션을 먼저 닫는다
    NewSession(ctx, scaleSetID int, owner string) (Session, error)
}

// Session 은 listener 가 요구하는 메시지 클라이언트에 세션 정리를 더한 것이다.
type Session interface {
    listener.Client
    Close(ctx context.Context) error
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
func (s *scaleSetScaler) HandleJobCompleted(ctx, *scaleset.JobCompleted) error // RunnerName(비면 RunnerID)으로 unit 을 찾아 msgJobCompleted → 살아 있으면 Completed=true, 이미 die 했으면 pendingCompletion 에서 제거 [§7.2-3]
```

확인된 listener 동작(v0.4.0 소스 기준):
- `JobAvailableMessages`를 **전부** `AcquireJobs`한다. [§7.2-2]
- 초기 세션의 `Statistics.TotalAssignedJobs`와 이후 **매 메시지**의 `TotalAssignedJobs`를 `HandleDesiredRunnerCount`에 넘긴다. 반환값은 메트릭 기록에만 쓴다.
- **빈 폴링**(long-poll 만료, `msg == nil`)에도 직전 메시지의 `Statistics`를 캐시해 **같은** `TotalAssignedJobs`로 `HandleDesiredRunnerCount`를 부른다(`listener/listener.go` 189~190행). 콜백 인자만으로는 새 통계와 캐시를 구분할 수 없으므로 Controller는 §7.2-3의 `pendingCompletion` 보정으로 완료분을 뺀다.
- 한 메시지 안의 호출 순서는 `DeleteMessage` → `AcquireJobs` → `HandleJobStarted`(각) → `HandleJobCompleted`(각) → `HandleDesiredRunnerCount`로 고정이다(`listener/listener.go` 207~237행). 완료를 반영한 통계와 그 `JobCompleted`는 같은 묶음에서 `JobCompleted`가 먼저 처리되므로, desired 계산 시점에 `pendingCompletion`은 이미 갱신돼 있다.
- Controller는 scale set별 `pendingCompletion`(runner 이름 → {등록 시각, 머신, RunnerID})과 unit별 `Completed`를 보유한다. 갱신 지점: `msgJobCompleted`(unit이 살아 있으면 `Completed=true`, 이미 die 했으면 집합에서 제거), busy·미완료 unit의 **Dying 전이**(`die`, tick 미등록 판정, 기동 타임아웃, 시작 실패, 재동기화 — 전이 함수 하나(`markDying`)에서 `Completed`면 넣지 않고, 아니면 집합에 추가), 세션 재시작(`msgSessionStarted`, 전부 비움), `msgResynced`(그 머신 소속 항목만 비움 — 항목의 머신 필드로 판별), `msgTick`(5분 만료 항목 제거, §8.3 상수 표). 이름 키라 중복 콜백에 멱등이다.
- RunnerID 대조: `JobCompleted`의 이름이 비면 살아 있는 unit의 id(`runnerIDs`, GenerateJIT 결과 또는 입양 unit의 첫 `GetRunner` 결과에서 `learnRunnerID`로 기록) → `pendingCompletion` 항목의 RunnerID 순으로 찾는다. 둘 다 없으면 `completedIDs`(RunnerID → 수신 시각)에 보관하고, `learnRunnerID`가 그 id를 알게 되는 시점에 적용한다(살아 있으면 `Completed=true`, 아니면 `pendingCompletion`에서 제거). `msgTick`이 5분 지난 항목을 버린다. [§7.2-3]
- `SetMaxRunners(n)`은 atomic 저장이며 다음 `GetMessage`의 `maxCapacity`에 반영된다. [§7.2-1]
- `listener.Config.Validate`는 `0 ≤ MaxRunners ≤ MaxInt32`를 요구한다. **MaxRunners 0이 허용**되므로 capacity 0인 scale set도 listener를 정상 시작하고, 운영 중 `SetMaxRunners(0)`도 허용된다. 중지·재시작 로직은 필요 없다.
- 초기 세션의 `Statistics`가 nil이면 `Run`이 오류를 반환한다. Controller는 listener 오류를 로그 후 이전 세션을 닫고 백오프(§7 백오프와 동일 수열)로 재시작한다. "성공 시 리셋"의 성공은 세션이 백오프 최대값(30s) 이상 유지된 것으로 본다(`Run`은 오류로만 끝나므로 다른 성공 신호가 없다). 유지 시간은 세션이 선 뒤부터 재고 세션 생성·정리 시간은 빼며(생성이 느리게 실패한 회차를 성공으로 세지 않는다), 이는 §7 회차의 스트림 유지 시간과 같은 기준이다. 세션 (재)시작마다 `msgSessionStarted`를 보내 `pendingCompletion`을 비운다(§7.2-3 안전장치 1).
- 세션은 `SetMaxRunners` 반영을 위해 `scaleSetState`가 원자 값(`capacity`, listener 핸들)으로 들고, listener goroutine은 생성 직후 저장된 capacity를 한 번 더 `SetMaxRunners`해 생성과 저장 사이의 변경을 흡수한다.

## 5. 순수 로직 (`internal/plan`)  [§8]

입력 타입은 전부 `domain` 패키지 것이다(`plan → domain → resource`). `plan`은 자체 입력 구조체를 두지 않는다. 예외는 `Observed`/`Action`/`ReconcileConfig`로, 이 셋만 `plan`이 정의한다.

```go
package plan

func PhysicalMax(machine, unit resource.Budget) int                                   // floor(min(cpu/cpu, mem/mem)) [§8.1]
func EffectiveMax(physical int, maxRunners *int) int                                  // [§8.1, R22]
func Capacity(ss domain.ScaleSet, machines []domain.Machine) int                      // Healthy 만 합산, ss.MaxRunners 로 cap [§8.1, R23]
func Desired(capacity, minRunners, assigned int) int                                  // min(cap, max(min, assigned)) [§7.2-3]
func Spread(machines []domain.Machine, occupied map[string]int, now time.Time) (string, bool) // occupied/effectiveMax 최저 → 여유 슬롯 → LastPlacedAt → 순서 [§8.2]
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

**책임 경계 (tick vs Reconcile)**: GitHub 등록 대조는 `msgTick`에서만 한다. `Reconcile`은 재동기화 시점의 **부품 집합만** 판정한다. 스냅샷에는 관측 시각이 있다(`machine.Snapshot.At` → `msgResynced.At`. `Observed`에는 없다): Controller는 그 시각 이후에 `Starting`이 된 unit을 §8.3 Creating 예외로 취급해 `known`에 `Creating`으로 넘긴다(스냅샷이 찍힐 때 만들던 중이었으므로 부품 부재가 정상이다). 그래서 `Observed`에 등록 여부가 없고, SPEC §8.3의 "runner 살아 있는데 등록 없음" 행은 `Reconcile`이 아니라 Controller의 tick 처리가 구현 주체다. 입양된 살아 있는 unit은 등록 여부를 보지 않고 `Starting`으로 들어가며(CreatedAt = 컨테이너 생성 시각), 다음 tick이 `GetRunner`로 `Running` 승격 또는 grace 초과 `Dying`을 판정한다(§3.2). `runtime.Container`를 입력으로 받기 위해 `plan → runtime`의 타입 의존이 생기지만 `runtime`의 I/O 함수는 호출하지 않는다.

입양 시 `Unit.Machine`은 도달한 머신(`Observed.Machine`)이지만 `RunnerName`은 라벨의 `gh-ars.scaleSet`/`gh-ars.machine`으로 만든다. 등록은 생성 당시 이름으로 되어 있어 그 이름이라야 tick의 `GetRunner` 대조와 정리 시 `RemoveRunner`가 맞는다(§4.3, §8.3). `known`에 있고 `Creating`인 unit은 판정에서 제외한다(§8.3 Creating 예외).

`occupied` 계산에서 `Foreign` unit은 머신 slot 1개, `Dying` unit은 부품이 남아 있는 동안 slot 1개로 센다. [§8.3] `Spread`는 여유 슬롯과 사용률을 모두 이 `occupied`로 계산한다(§8.2). scale set 단위 `running` 집계는 `Desired` 쪽 입력이고 배치에는 쓰지 않는다.

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
    msgHealth       struct{ Machine string; Health domain.Health; Err error } // Unhealthy(재접속 대상) | Failed(§3.3, 재접속 없음)
    msgResynced     struct{ Machine string; Obs plan.Observed }
    msgTick         struct{}                                   // 30s 코드 상수 [§8.3 상수 표]
    msgUnitStarted  struct{ Unit UnitID; Err error }           // 비동기 create→cp→start 완료
    msgJobStarted   struct{ ScaleSet, RunnerName string }      // listener HandleJobStarted → Busy=true
    msgJobCompleted struct{ ScaleSet, RunnerName string; RunnerID int64 } // listener HandleJobCompleted → Completed / pendingCompletion 갱신 [§7.2-3]
    msgDrainResult  struct{ Unit UnitID; Busy bool; Err error } // 비동기 RemoveRunner(축소) 결과
    msgCleanupDone  struct{ Unit UnitID; Err error }           // 비동기 cleanupUnit 결과
    msgRegistration struct{ Unit UnitID; Found bool; RunnerID int64; Err error } // msgTick 이 goroutine 으로 뺀 GetRunner 결과. 판정 규칙은 msgTick 행
    msgSessionStarted struct{ ScaleSet string }                 // 메시지 세션 (재)시작 → pendingCompletion 비움 [§7.2-3 안전장치 1]
)

`msgUnitStarted`는 `ScaleSet`·`RunnerName`·`RunnerID`도 싣는다: unit이 그 사이 죽었거나 정리됐어도 `pendingCompletion`·`completedIDs` 대조에 id가 필요하다(§4.5). `msgResynced`는 관측 시각 `At`를 싣는다(§5 책임 경계).

Controller가 머신에 요구하는 것은 `MachineAgent` 인터페이스(`Name`, `Runtime`, `Preflight`, `Run`)다. 구현은 `*machine.Agent`, 테스트는 fake. 느린 작업은 전부 goroutine이며 루프에 넘길 값은 **루프에서 복사해** 넘긴다(goroutine 안에서 상태를 역참조하지 않는다).
```

루프는 `select` 하나로 메시지를 처리한다. 메시지별 후속 동작:

| 메시지 | 동작 |
|---|---|
| `msgDesired` | capacity 재계산 → 바뀌면 `SetMaxRunners`. `desired = Desired(capacity, minRunners, max(0, Assigned − len(pendingCompletion)))`(§7.2-3). `create = desired − running > 0`이면 **신규 unit 생성은 여기서만**: spread(occupied 기준)로 머신 선택 → `Creating` 등록 → goroutine `startUnit`. `remove = running − desired > 0`이면 **축소도 여기서만**: `plan.ScaleDown`으로 후보 선정 → `Draining` → goroutine으로 `RemoveRunner`(결과는 `msgDrainResult`). Reply에 running 목표를 보낸다. [§7.2-3] |
| `msgDrainResult` | 성공: `Draining` 유지, `die`를 기다린다. `Busy`(IsBusy) 또는 기타 오류: `Running`으로 복귀(오류는 로그). unit이 이미 `Dying`이면 무시. [§7.2-3] |
| `msgJobStarted` | RunnerName으로 unit을 찾아 `Busy=true`. 못 찾으면 로그만. |
| `msgJobCompleted` | RunnerName(비면 RunnerID)으로 unit을 찾는다. 살아 있으면 `Completed=true`. 없거나 이미 `Dying`/제거됐으면 그 scale set의 `pendingCompletion`에서 이름을 뺀다(없으면 무시). [§7.2-3] |
| `msgEvent` (`die`, role=runner) | unit(Creating/Starting/Running/Draining 모두)을 `markDying`으로 `Dying`으로 → goroutine `cleanupUnit`. `markDying`은 Dying 전이의 유일한 경로이며 `Busy && !Completed`면 RunnerName을 `pendingCompletion`에 넣는다(등록 시각·머신·RunnerID 기록) — tick 미등록 판정 등 다른 경로로 죽어도 같은 보정을 받는다. 그 뒤 `minRunners` 미달분만 보충하고 **assigned 기반 신규 생성은 하지 않는다**(§7.2-3 식의 귀결). 이미 `Dying`/정리된 unit의 die는 무시. [§7.2-3, §7.2-5] |
| `msgEvent` (`die`, role=sidecar) | runner가 살아 있으면 로그만(runner die 시 함께 정리). |
| `msgHealth` | 머신 health 갱신 → capacity 재계산 → `SetMaxRunners`. unhealthy 머신의 Dying unit은 정리를 보류한다. `Failed`(재접속 preflight의 R16/R21 위반)는 되돌리지 않는다: 이후의 health·재동기화 메시지로도 healthy로 복귀하지 않는다(§3.3). |
| `msgResynced` | **부품은 스냅샷이 권위다.** `plan.Reconcile` 실행 **전에** 이 머신 소속 known unit의 `Parts`를 `Obs`가 보여준 부품 집합으로 덮어쓴다(정리 실패나 외부 rm으로 캐시가 어긋나면 §8.3의 slot 점유 계산이 틀어진다). `State`는 유지한다 — 상태 판정에는 GitHub 등록 대조가 필요해 `Reconcile`이 판정하지 않는다(§5 책임 경계). `Busy`도 유지한다 — 출처가 머신 이벤트가 아니라 메시지 세션(`msgJobStarted`)이라 SSH 단절로 낡지 않고, 되채우는 경로가 없어 리셋하면 남은 생애 동안 `false`로 고정된다. 그 다음 `Reconcile` 결과의 Adopt/RemoveUnit/RemoveOrphan을 적용한다(보류된 Dying 포함). RemoveOrphan은 unit id별로 모아 **runner 이름 없는 Dying unit**으로 등록한다(`Mode=sidecar`, `Foreign`, 관측된 Parts): `cleanupUnit`의 GitHub 단계만 건너뛰고 sidecar → 볼륨 → slice 순서, tick 재시도, slot 점유를 그대로 탄다(고아 등록은 GitHub이 자동 제거, §3.2). 관측 시각(`At`) 이후 `Starting`이 된 unit은 Creating 예외(§5). 부품 집합만 판정하고 GitHub은 조회하지 않는다. 이 머신 소속 `pendingCompletion` 항목을 비운다(§7.2-3 안전장치). 머신 healthy 복귀. |
| `msgTick` | **GitHub 등록 대조의 유일한 구현 주체.** Starting/Running unit(**Draining 제외**, 대조가 진행 중이 아닌 것)을 모아 goroutine이 `GetRunner`를 부르고 결과를 `msgRegistration`으로 돌려보낸다(루프 안에서 GitHub을 부르지 않는다). `Creating`의 기동 타임아웃 판정. healthy 머신의 `Dying` unit 중 정리가 진행 중이 아닌 것은 `cleanupUnit` 재시도. `pendingCompletion`·`completedIDs`에서 5분 지난 항목 제거(§7.2-3 안전장치, §8.3 상수 표). [§8.3] |
| `msgRegistration` | `Found`면 `learnRunnerID`(§4.5). Starting: 등록되면 `Running`, 미등록이고 grace 초과면 `Dying`, 이내면 대기. Running: 미등록이면 grace 없이 `Dying`. 오류는 로그만(다음 tick 재시도). unit이 그 사이 다른 상태가 됐으면 무시. [§8.3] |
| `msgUnitStarted` | 먼저 `learnRunnerID`(unit이 이미 죽었거나 정리됐어도 id는 `pendingCompletion`·`completedIDs` 대조에 쓴다). 성공: `Creating → Starting`(전이 시각 기록 — §5 Creating 예외 판정용). 실패: `Dying`으로 두고 `cleanupUnit`(역순 정리, RemoveRunner 포함). 다음 `msgDesired`에서 재배치. unit이 이미 `Dying`이면 상태는 바꾸지 않는다. |
| `msgCleanupDone` | 성공: `Removed`(상태에서 삭제, slot 해제). 실패: `Dying` 유지, 다음 tick에서 재시도. |

`startUnit` (goroutine, 상태를 직접 만지지 않는다) [§7.2-4]:
```
JIT 생성(GitHub) → [sidecar: slice Create, 볼륨 3개 Create, sidecar 컨테이너 Create+Start]
→ runner 컨테이너 Create(entrypoint 래퍼, 라벨, 리소스) → CopyIn(jittar) → Start
```
전체에 2분 타임아웃 컨텍스트. 실패 시 §3.2 정리 순서(GetRunner → RemoveRunner 포함)로 만든 부품을 정리하고(분리한 ctx, 정리 회차 상한 2분) `msgUnitStarted{Err}`. 단 **프로세스 종료(부모 ctx 취소)로 중단된 경우는 되돌리지 않는다** — 컨테이너가 이미 start 됐을 수 있고 §7.3은 실행 중 컨테이너를 kill하지 않는다. 재시작 시 입양 또는 exited 정리(§8.3).

`cleanupUnit` (goroutine): §3.2 정리 순서 실행(1회 시도 상한 2분, §8.3 상수 표) → `msgCleanupDone{Err}`. 실패해도 부분 정리된 부품은 다음 시도에서 건너뛴다(각 단계는 "없으면 성공"으로 멱등). RunnerName이 빈 unit(고아 부품 집합)은 GitHub 단계를 건너뛴다.

## 7. Machine 에이전트 (`internal/machine`)  [§7.1, §10]

머신마다 goroutine 하나. 책임: 접속 유지, preflight, pre-pull, `Events` 스트림 수신 → `msgEvent`, 단절 시 백오프 재접속 → 재접속 후 `Observe()`(ps -a, 볼륨, slice) → `msgResynced`. Controller는 `Agent.Runtime()`, `Agent.Slices()`로 명령을 보낸다.

`Run(ctx, sink)`의 회차: `Events` 열기 → `info`(데몬 생존) → `Observe`(관측 시각 기록) → `Resynced` → 스트림 소비. **events를 먼저 열고 관측한다**(반대면 그 사이의 die를 놓친다). 열기 직후 스트림이 이미 끝나 있으면 `Resynced`를 보내지 않고 실패로 본다. `Resynced`를 보낸 회차라도 백오프 리셋은 스트림이 백오프 최대값(30s) 이상 유지된 경우에만 한다(§6 listener 세션과 동일 기준). 유지 시간은 회차 전체가 아니라 events 스트림이 열려 있던 시간으로 잰다(pre-pull이 오래 걸린 회차가 스트림 즉사에도 리셋 자격을 얻으면 안 된다). 스트림이 끝나면 `Unhealthy` → 백오프 → 다음 회차. 첫 회차는 `Resynced` 또는 `Unhealthy` 중 하나를 반드시 보내며, Controller는 시작 시 머신마다 그 첫 통지를 기다린 뒤 메시지 세션을 연다(§7.1-7·8 → §7.1-9 순서. 첫 desired로 만든 unit의 die를 events가 받아야 한다). 시작 시 §7.1-7 동기화도 이 첫 회차의 `Resynced`다.

preflight 순서 (SPEC §10.2 판단 규칙과 §7.1-3):
1. SSH 머신: host key 검증 후 접속(타임아웃 10s). local: 생략.
2. `id -u` → root 여부.
3. runtime `info`:
   - docker: `docker info`(sudo 없음). 실패 → none은 unhealthy, sidecar는 R16 오류.
   - podman: `podman info` → 실패면 `sudo -n podman info`(모드 무관). 성공한 경로를 `Machine.Sudo`로 고정. 둘 다 실패 → none은 unhealthy, sidecar는 R16 오류. sidecar이고 고정 경로의 `Rootless=true`면 아직 시도하지 않은 `sudo -n podman info`를 시도해 rootful이면 `Sudo=true`로 바꾸고, 그래도 rootless면 R16 오류. [§10.2 규칙 3]
4. sidecar scale set이면: `CgroupDriver == systemd && CgroupVersion == 2` 확인, `Slices.Check()`(root 아니면 sudo -n) → 실패 시 R16 오류.
5. resources: 생략 시 `info`의 CPUs/MemoryBytes, 명시 시 탐지값으로 cap(R21 경고). physicalMax·effectiveMax 계산(R21 오류, R22 경고).
6. pre-pull: runner 이미지, sidecar면 sidecar 이미지. 실패 → unhealthy.

2~5단계의 확인 명령과 관측(`ps`, `volume ls`), events 스트림 **열기**에는 probe 상한(§8.3 상수 표)을 건다. 열기에만 거는 이유는 스트림 자체가 장기 실행이기 때문이고, 상한은 열기 구간에만 타이머로 스트림 ctx를 취소해 건다. 6단계 pull은 이미지 크기에 비례하므로 별도의 pre-pull 상한(§8.3)을 쓴다.

시작 시와 재접속 시의 차이: 시작 시 R16/R21 오류는 프로세스 시작 실패. 재접속 후 preflight에서 같은 오류가 나면 `msgHealth{Failed}`를 보내고 에이전트 goroutine을 종료한다(재접속 없음). R24는 Controller가 시작 시 한 번 평가한다: 전 머신 도달이면 위반 시 시작 실패, 미도달 머신이 있으면 경고. [§6.2 R21, R24]

`NewDocker/NewPodman(ex, sudo, log)`와 `systemd.New(ex, sudo)`의 `sudo`는 위 2·3·4단계 결과에서 나온다(docker: 항상 false, podman: 3단계 결과, systemd: `!root`).

5단계의 예산 판정(R21)만은 설정 값을 아는 Controller가 `Spec.Verify(info) error`로 넘긴다(machine은 config에 의존하지 않는다, §2). 에이전트는 이 오류를 R16과 같은 등급(`ErrFatal`)으로 다뤄 시작 시에는 그대로 올리고 재접속 후에는 `Failed`로 만든다. 접속 재료도 같은 이유로 `machine.SSH`(config.SSH의 값 복사)로 받는다.

백오프: 1s 시작, ×2, 최대 30s, ±20% jitter, 성공 시 리셋(성공의 기준은 위 회차 문단). local 머신은 events 재시작에만 적용. [§7.1-8]

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
