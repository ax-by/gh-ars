# gh-ars — GitHub Actions Runner Scale Set Autoscaler (MVP 기획서)

상태: 구현 기준 문서. 미결 사항 없음. 설정 파일 전체 예시(주석 포함)는 리포의 `examples/gh-ars.yaml` 참고.

---

## 1. 배경과 목표

- gh-ars는 **K8s 없이**, 이미 존재하는 정적 머신(EC2/Azure VM/GCP VM/OCI VM/사내 서버/PC/노트북) 위에서 docker 또는 podman 컨테이너로 **ephemeral self-hosted runner를 오토스케일링**하는 Go CLI다.
- GitHub 통신은 `github.com/actions/scaleset` 클라이언트로 Runner Scale Set API(long-poll)를 사용한다.
- 1차 사용자: 개인과 팀. 유휴 머신을 CI에 활용하는 것이 동기. 정적 머신부터 제공하고, 프로비저닝은 후속 과제.

## 2. 배포 모델

- gh-ars는 **포그라운드 프로세스**다. 스스로 데몬화하지 않으며 터미널이 닫히면 종료된다.
- Scale Set API가 long-poll이므로 프로세스가 죽으면 job을 받지 못한다. 상시 실행은 systemd·컨테이너 등 **실행 환경의 책임**이다.
- **설정 파일 1개 = GitHub 대상(org 또는 repo) 1개 = 프로세스 1개.** 파일 안에 `scaleSets[]` 여러 개 가능. 다른 org/repo는 파일과 프로세스를 분리한다.
- scaler와 runner 머신은 원칙적으로 분리하며 머신 제어는 **SSH로만** 한다.
- **local executor**: `machines[]` 항목에 `host`를 주지 않으면 gh-ars가 실행 중인 머신에서 SSH 없이 직접 실행한다. `ssh` 설정은 무시되며, 설정 파일당 최대 1개(§6.2 R17, R18).
- Thin control: 원격 머신에 gh-ars 바이너리/에이전트를 배포하지 않는다. SSH 위에서 `docker`/`podman`/`systemctl` CLI만 호출한다.
- gh-ars는 이미지를 빌드·배포하지 않는다. 기본 runner 이미지는 `ghcr.io/actions/actions-runner:2.337.0`(코드 상수, 릴리스마다 갱신). 사용자가 `runner.image`로 커스텀 이미지를 지정할 수 있다. 시작 시 각 머신에 미리 pull한다(§7.1).

## 3. 범위

### 3.1 MVP 포함
- repo / org scope. `github.url` 형태로 판별. GHES는 서버 URL 그대로 사용.
- 인증: PAT(`github.auth.token`) 또는 GitHub App(`github.auth.app.*`) 중 하나. 둘 다 있으면 오류. secret은 `${env:}`/`${file:}` 참조로만.
- ephemeral runner (job 1개 실행 후 종료), JIT config 기반 등록.
- 컨테이너 runtime: `machineDefaults.runtime` 기본값 docker, 머신별 `runtime`으로 podman override. (runtime 자동 탐지는 없다. 머신 리소스 자동 탐지와 구분할 것.)
- `jobRuntime.mode: none | sidecar` **둘 다 구현** (sidecar = ARC dind 상당, podman-in-podman 변형 포함, systemd transient slice 기반 cgroup 예산).
- local executor + SSH executor.
- 다중 머신 spread 배치, 동적 capacity 보고, warm runner(minRunners).
- 시작 시 전체 동기화 + 이벤트 스트림 기반 상태 갱신 + 재시작 시 unit 입양 + 짝 안 맞는 부품 정리.
- CLI: `run`, `scaleset delete` (2개).
- 설정 파일 strict decode, `${env:}` / `${file:}` 치환(디코드 후 struct 필드 값 단위, §6.0).

### 3.2 Non-goal (문서에 명시)
- scale set의 runner group 간 이동 (§7.1-5 각주)
- unit id를 알 수 없는 고아 GitHub 등록의 정리 (§8.3. GenerateJIT 직후·컨테이너 create 전에 gh-ars가 죽은 경우. GitHub이 자동 제거한다: JIT runner는 job을 한 번도 실행하지 않으면 제거되고, ephemeral runner는 1일 이상 미접속이면 제거된다. 그 전에 지우고 싶을 때만 GitHub UI/API에서 직접 제거)
- enterprise scope
- 머신 프로비저닝(동적 생성/삭제)
- 이미지 빌드·배포
- ssh-agent, 비밀번호·인증서 SSH 인증, `~/.ssh/config` 별칭 해석, TOFU
- 한 머신의 다중 scale set 소속
- systemd 없는 머신의 sidecar 모드 (none 모드는 동작)
- Windows/macOS 호스트, Windows 컨테이너 runner
- 설정 hot reload
- 여러 org/repo 동시 운영 (프로세스 분리로 대체)
- job별 리소스 조정
- 가짜 Scale Set 서버, 자동화된 통합 테스트, 메트릭/웹 UI
- 타임아웃·grace 상수의 설정 노출 (§8.3 상수 표의 값들은 코드 상수. 노출은 후속 과제)
- pre-pull의 무진행(idle) 감지 (MVP는 §8.3의 pre-pull 상한 하나로 막는다. 정상적으로 느린 pull을 살리면서 응답 없는 pull만 끊으려면 총량이 아니라 "출력이 60s 이상 없으면 중단"이 맞다. 후속 과제)

## 4. 핵심 개념

### 4.1 unit
- **unit = runner 하나의 실행 인스턴스.** scale set도 YAML 블록도 아니다.
- run 1회(= JIT config 1개 = job 1개)마다 새 unit id(**ULID**, 26자)를 발급한다.
- 구성:
  - `none` 모드: runner 컨테이너 1개
  - `sidecar` 모드: runner 컨테이너 + sidecar 컨테이너 + 볼륨 3개(`work`, `sock`, `externals`) + systemd slice 1개
- 각 unit은 정확히 하나의 scale set과 하나의 머신에 속한다. scale set당 unit 수는 0..capacity.

### 4.2 라벨 세트 (설정 파일 없이 reconcile 가능해야 함)

| 라벨 | 값 |
|---|---|
| `gh-ars.unit` | ULID |
| `gh-ars.role` | `runner` \| `sidecar` |
| `gh-ars.scaleSet` | scale set 이름 |
| `gh-ars.mode` | `none` \| `sidecar` |
| `gh-ars.machine` | 머신 이름 (로그용. 소속 머신의 권위는 "SSH로 도달한 현재 머신" 또는 local) |

라벨은 events 스트림의 **구독 선택**에도 쓴다(`events --filter label=gh-ars.unit --filter type=container`): 스트림에 무엇이 실릴지는 라벨이 고르고, 그 안에서 unit/role을 **식별**하는 것은 컨테이너 이름이다(§7.1-8). 볼륨에도 같은 라벨을 붙이되 `gh-ars.role`은 제외한다. 볼륨은 runner와 sidecar가 함께 마운트하는 unit 단위 부품이라 role 값이 없다(§9.1). 볼륨 조회는 특정 unit 정리 시 `gh-ars.unit=<unit>`, 머신 전체 동기화 시 `gh-ars.unit` 키 존재로 한다(후자는 고아·Foreign unit을 포함해야 하므로 값이나 scale set으로 거르지 않는다). slice 이름은 `gh-ars-<unit>.slice`.

**이름과 라벨의 역할 분담**: events 스트림에서는 컨테이너 이름 `gh-ars-<unit>-runner|sidecar`(§4.3)로 unit과 role을 식별한다. 이름은 docker/podman 두 runtime의 `die` 이벤트에 항상 포함되기 때문이다. 라벨은 스트림 구독 선택(`events --filter label=`), `ps --filter` 조회, 볼륨·slice 대조, 입양 시 scale set/mode 복원에 쓴다 — 무엇을 받을지는 라벨이 고르고, 받은 것이 어느 unit인지는 이름이 정한다.

### 4.3 명명
- GitHub runner 이름: `<scaleSet>-<machine>-<unitId>`. GitHub 등록 ↔ 머신 컨테이너 1:1 대조 키. 64자 제한 → `len(scaleSet)+len(machine) ≤ 36` (§6.2 R14).
- 컨테이너 이름: runner `gh-ars-<unit>-runner`, sidecar `gh-ars-<unit>-sidecar`.
- 볼륨 이름: `gh-ars-<unit>-work|sock|externals`.

## 5. 아키텍처

```
┌────────────────────────── gh-ars run (foreground) ───────────────────────────┐
│  Config loader / validator (strict decode, ${env:}/${file:} 치환)            │
│  GitHub layer (actions/scaleset): scale set ensure, session, GetMessage,     │
│      AcquireJobs, GenerateJitRunnerConfig, RemoveRunner, DeleteMessage      │
│  Scheduler: capacity 계산, desired 계산, spread 배치, pending 큐              │
│  Reconciler: 전체 동기화, 이벤트 반영, 입양, 고아 정리                        │
│  Executor 인터페이스 ──┬── local  (직접 exec)                                 │
│                        └── ssh    (keyFile, host key 강제 검증)               │
│      요구사항: 명령 실행 + stdin 파이프 전달 + 장기 스트림(events) 지원        │
│  Runtime 인터페이스 ───┬── docker                                             │
│                        └── podman                                            │
│      요구사항: ps/create/cp/start/rm/pull/info/events 를 공통 모델로 정규화     │
└──────────────────────────────────────────────────────────────────────────────┘
        │ SSH (또는 local exec): docker/podman/systemctl CLI + events 스트림
        ▼
  머신 N: [runner 컨테이너] (+ [sidecar] + 볼륨 3 + slice)  … unit 단위
```

- **진실(source of truth)**: "몇 개 실행 중인가"는 각 머신의 컨테이너 runtime. scaler 메모리는 캐시.
  "몇 개 필요한가"는 GitHub `Statistics.TotalAssignedJobs`.
- **인터페이스 분리**: GitHub 클라이언트, Executor(SSH/local), Runtime(docker/podman)을 인터페이스로 두어 유닛 테스트에서 대체 가능하게 한다.
- **events 스트림의 runtime 차이**: `docker events --format '{{json .}}'`와 `podman events --format json`은 JSON 필드명이 다르다. Runtime 인터페이스가 이를 흡수해 `{unit, role, event, exitCode}` 공통 모델로 낸다. unit/role은 두 runtime 모두 `die` 이벤트에 항상 포함되는 **컨테이너 이름** `gh-ars-<unit>-runner|sidecar`에서 파싱한다(§4.2). 라벨 Attributes에 의존하지 않는다.
- **Executor 전달 방식**: 명령 실행, stdin 전달(§7.2-4의 `docker cp -` tar 스트림용), 장기 스트림(events)을 지원하도록 설계한다.

## 6. 설정 파일

### 6.0 필드 표

타입의 `cpu`는 숫자 또는 문자열(`2`, `0.5`, `"2"`)을 허용하고 내부에서 float으로 정규화한다. `memory`는 `Ki/Mi/Gi` 접미 문자열이며 앞자리는 양의 정수다(R25).

| 필드 | 타입 | 필수 | 기본값 | 규칙 |
|---|---|---|---|---|
| `github.url` | string | 필수 | – | R6 |
| `github.auth.token` | string(참조) | 택1 | – | R3, R4, R5 |
| `github.auth.app.clientId` | string | 택1 | – | R3 |
| `github.auth.app.installationId` | int64 | 택1 | – | R3 |
| `github.auth.app.privateKey` | string(참조) | 택1 | – | R3, R4, R5 |
| `scaleSets[].name` | string | 필수 | – | R8, R14 |
| `scaleSets[].runnerGroup` | string | 선택 | `Default` | R7 |
| `scaleSets[].minRunners` | int | 선택 | `0` | R24 |
| `scaleSets[].maxRunners` | int | 필수 | – | R23 |
| `scaleSets[].resources.cpu` | number\|string | 필수 | – | R25 |
| `scaleSets[].resources.memory` | string | 필수 | – | R25 |
| `scaleSets[].runner.image` | string | 선택 | `ghcr.io/actions/actions-runner:2.337.0` | R13 |
| `scaleSets[].jobRuntime.mode` | `none`\|`sidecar` | 선택 | `none` | R15, R16 |
| `scaleSets[].jobRuntime.image` | string | 선택 | runtime별 기본(§9) | R13 |
| `machineDefaults.ssh.user` | string | 선택 | – | – |
| `machineDefaults.ssh.port` | int | 선택 | `22` | – |
| `machineDefaults.ssh.keyFile` | string | SSH 머신 있으면 필수 | – | R19 |
| `machineDefaults.ssh.keyPassphrase` | string(참조) | 선택 | – | R4, R5, R19 |
| `machineDefaults.ssh.knownHostsFile` | string | 선택 | `~/.ssh/known_hosts` | R20 |
| `machineDefaults.ssh.insecureSkipHostKeyVerify` | bool | 선택 | `false` | R20 |
| `machineDefaults.runtime` | `docker`\|`podman` | 선택 | `docker` | R12 |
| `machines[].name` | string | 필수 | – | R9, R14 |
| `machines[].scaleSet` | string | 필수 | – | R10, R11 |
| `machines[].host` | string | 선택 | (없으면 local) | R17, R18 |
| `machines[].ssh.*` | machineDefaults.ssh 와 동일 + `fingerprint` | 선택 | machineDefaults | R17, R19, R20 |
| `machines[].runtime` | `docker`\|`podman` | 선택 | machineDefaults.runtime | R12, R15 |
| `machines[].maxRunners` | int | 선택 | physicalMax | R22 |
| `machines[].resources.cpu/memory` | 위와 동일 | 선택 | runtime `info` 자동 탐지 | R21, R25 |

알 수 없는 키는 오류(strict decode, R2).

`${env:NAME}` / `${file:PATH}` 치환은 **YAML 디코드 후 struct 필드 값에 대해** 수행한다. 원문 텍스트 치환은 금지한다. `${file:}`로 여러 줄 PEM이 들어오면 YAML 구조가 깨지기 때문이다. 참조 형식 검사(R4)도 디코드된 필드 값에서 한다.

### 6.1 예시 (축약본. 필드별 설명이 달린 전체 예시는 `examples/gh-ars.yaml`)

```yaml
github:
  url: https://github.com/my-org               # org scope. repo: https://github.com/my-org/my-repo
  auth:
    token: "${env:GITHUB_PAT}"                 # 또는 app: {clientId, installationId, privateKey: "${file:...}"}

scaleSets:
  - name: linux-x64                            # == runs-on. none 모드: docker/podman 머신 혼합 가능
    runnerGroup: Default
    minRunners: 0
    maxRunners: 10
    resources: { cpu: 2, memory: 4Gi }         # unit 1개 예산 (sidecar 면 runner+sidecar 합산)
    runner:
      image: ghcr.io/actions/actions-runner:2.337.0
    jobRuntime:
      mode: none

  - name: linux-x64-dind                     # sidecar 모드: 연결 머신 runtime 전부 동일(rootful docker)
    runnerGroup: Default
    minRunners: 0
    maxRunners: 4
    resources: { cpu: 4, memory: 8Gi }
    runner:
      image: ghcr.io/actions/actions-runner:2.337.0
    jobRuntime:
      mode: sidecar
      # image: docker:29.7.2-dind              # 생략 시 runtime별 기본 이미지

machineDefaults:
  ssh:
    user: runner
    port: 22
    keyFile: ~/.ssh/ci
    knownHostsFile: ~/.ssh/known_hosts
    insecureSkipHostKeyVerify: false
  runtime: docker

machines:
  - name: build-1
    scaleSet: linux-x64
    host: 10.0.0.12
    ssh:
      fingerprint: "SHA256:xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
      # keyPassphrase: "${env:CI_KEY_PASS}"
    runtime: podman
    maxRunners: 2
    resources: { cpu: 4, memory: 8Gi }      # 명시 → 탐지값을 상한으로 cap (R21)
  - name: build-2
    scaleSet: linux-x64-dind               # sidecar scale set: rootful docker 머신
    host: 10.0.0.13
    runtime: docker                          # resources 생략 → runtime info 로 자동 탐지
```

`host`, `ssh`를 생략한 머신 항목은 local executor로 동작한다.

### 6.2 검증 규칙 (전부 유닛 테스트 대상)

"오류"는 시작 실패. "경고 후 cap"은 로그 후 계속.

| # | 규칙 |
|---|---|
| R1 | (삭제됨. `version` 필드를 두지 않기로 결정. 스키마 변경은 R2 strict decode와 필수 필드 누락 오류로 감지) |
| R2 | 알 수 없는 키 → 오류 (strict decode) |
| R3 | `auth.token`과 `auth.app` 동시 설정 → 오류. 둘 다 없음 → 오류. `app`은 세 필드 모두 필수 |
| R4 | `auth.token`, `auth.app.privateKey`, `ssh.keyPassphrase`에 `${env:}`/`${file:}` 참조가 아닌 리터럴 → 오류 |
| R5 | `${env:}`/`${file:}` 참조 대상 없음(빈 env 포함) → 오류 |
| R6 | `github.url`로 org/repo 판별 불가 또는 enterprise 형태 → 오류 |
| R7 | repo scope에서 `runnerGroup`이 `Default`가 아님(대소문자 무시 비교. 클라이언트 상수는 `"default"`) → 오류 |
| R8 | `scaleSets[].name` 중복 → 오류 |
| R9 | `machines[].name` 중복 → 오류 |
| R10 | `machines[].scaleSet` 참조 대상 없음 → 오류 |
| R11 | 연결된 머신이 0개인 scale set → 오류 |
| R12 | `runtime` 값이 `docker`/`podman` 외 → 오류 |
| R13 | `runner.image` / `jobRuntime.image`가 `:latest` 또는 태그 없음 → 오류. 다이제스트 참조(`@sha256:<64 hex>` / `@sha512:<128 hex>`)는 태그 유무와 무관하게 허용(다이제스트가 버전을 고정한다) |
| R14 | `len(scaleSet.name) + len(machine.name) > 36` → 오류 (runner 이름 64자 제한) |
| R15 | sidecar scale set에 연결된 머신의 runtime이 모두 같지 않음 → 오류. none scale set은 혼합 허용 |
| R16 | sidecar scale set 머신 preflight: rootless podman / systemd 없음 / cgroup v2 아님 / cgroup 드라이버 systemd 아님 / 권한 부족(§10.2) → 오류. none scale set의 rootless podman은 허용 |
| R17 | `host` 없는 머신에 `ssh` 블록 → 오류 (모순 설정) |
| R18 | `host` 없는 머신이 설정 파일당 2개 이상 → 오류 (같은 물리 머신 이중 계산) |
| R19 | `keyPassphrase` 유무와 키 파일 암호화 여부 불일치 → 오류. SSH 머신이 있는데 `keyFile` 없음 → 오류 |
| R20 | host key: `machines[].ssh.fingerprint` → `knownHostsFile` → 둘 다 없으면 접속 거부. 우회는 `insecureSkipHostKeyVerify: true`뿐이며 시작 시 경고 |
| R21 | `machines[].resources` < `scaleSet.resources` (physicalMax 0) → 오류. 명시값 > 탐지값 → 경고 후 탐지값으로 cap. resources를 생략한 머신은 도달해야 판정할 수 있으므로, 시작 시 미도달이었다가 재접속 후 위반이 드러나면 그 머신을 `Failed`로 영구 제외(재접속 대상에서도 제외)하고 오류 로그. 프로세스는 계속 |
| R22 | `machines[].maxRunners > physicalMax` → 경고 후 cap |
| R23 | `scaleSet.maxRunners` 없음 → 오류. `> Σ effectiveMax` → 경고 후 cap |
| R24 | `minRunners > capacity` → 시작 시 도달한 머신만으로 계산. 전 머신이 도달했는데 위반이면 오류. 미도달 머신이 하나라도 있으면 오류 대신 경고(desired가 `min(capacity, …)`라 동작은 깨지지 않고 warm runner가 덜 뜰 뿐) |
| R25 | `cpu` 파싱 실패(음수, 0, 숫자 아님) / `cpu < 0.01`(= `CPUQuota` 1% 미만) / `memory`가 `<양의 정수><Ki\|Mi\|Gi>` 형식이 아님(소수 `1.5Gi`도 오류. `1536Mi`로 쓴다) → 오류. 하한을 두는 이유: 그 아래는 systemd의 `CPUQuotaPerSecUSec` 반올림에 묻혀 slice에 실제로 적용되지 않고, §8.1의 `floor(machine.cpu / unit.cpu)`가 비현실적인 capacity를 낸다. 하한 위에서는 값을 클램프하지 않고 `cpu×100`을 그대로 쓴다(`cpu: 0.125` → `CPUQuota=12.5%`) |

## 7. 실행 흐름

### 7.1 시작 (`gh-ars run -c config.yaml`)
1. 설정 로드 → 치환 → 검증(R2~R15, R17~R20, R23~R25 중 정적 규칙).
2. GitHub 인증 및 scope 확인.
3. 각 머신 preflight:
   - SSH 머신: host key 검증 후 접속. local 머신: SSH·host key 단계 생략.
   - 설정된 runtime CLI 동작 확인(`docker info` / `podman info`, `--format '{{json .}}'` 한 번). runtime을 탐지하지는 않는다. 아래 항목들(예산 탐지, rootless, cgroup)은 모두 이 한 번의 결과에서 읽는다.
   - `machines[].resources` 생략 시 `info`로 CPU/메모리 자동 탐지, 명시 시 cap(R21). physicalMax·effectiveMax 계산(R22).
   - podman이면 같은 `info` 결과의 `host.security.rootless`로 rootless 확인. sidecar scale set이면 R16의 systemd/cgroup/권한(§10.2) 확인.
   - 분류: 설정·환경 모순(R16, R21 오류)은 **시작 실패**. 도달 불가·명령 실패는 **unhealthy**로 표시하고 배치에서 제외(프로세스는 계속, 재접속 시 재시도). 재접속 후 preflight에서 R16/R21 위반이 드러나면 시작 실패 대신 그 머신을 **`Failed`**(영구 제외, 재접속 안 함)로 두고 오류 로그.
4. 이미지 pre-pull: 각 healthy 머신에 `runner.image`를 pull. sidecar scale set이면 `jobRuntime.image`(또는 runtime별 기본 이미지)도 pull. 실패 머신은 unhealthy. pull 하나가 상한(§8.3)을 넘기면 실패로 본다 — preflight는 그 머신의 첫 통지(§7.1-7)보다 앞이므로, 상한이 없으면 응답 없는 pull 하나가 모든 scale set의 세션 시작(§7.1-9)을 무한정 막고 재접속 회차도 같은 자리에서 멈춰 그 머신이 unhealthy에 갇힌다.
5. scale set 확보: `runnerGroup`을 `GetRunnerGroupByName`으로 조회 → 그 그룹 안에서 이름으로 `GetRunnerScaleSet(groupID, name)` → 없으면 `CreateRunnerScaleSet`. 그룹 이동 분기는 없다.[^group] **종료 시 삭제하지 않는다.**

[^group]: 조회가 그룹 단위라 다른 그룹에 있는 동명 scale set은 발견되지 않으므로 "있으면 그룹 이동" 분기는 도달 불가다. 그룹 이동은 non-goal(§3.2). 다른 그룹의 동명 scale set과 이름이 충돌하면 GitHub이 생성을 거부하고 gh-ars는 시작 실패한다.
6. capacity 계산(R23, R24 동적 검증. R24는 도달한 머신 기준, 미도달 머신이 있으면 경고만).
7. 전체 동기화: 머신마다 `ps -a --filter label=gh-ars.unit`(종료된 컨테이너 포함)로 unit 입양, 고아 정리(§8.3).
8. events 스트림: SSH 머신은 유지되는 SSH 연결 위에, local 머신은 직접 `events`를 연다. 이벤트의 unit/role 식별은 컨테이너 이름 `gh-ars-<unit>-runner|sidecar`로 하고(§4.2), 라벨은 스트림 구독 선택(`--filter label=`)과 `ps --filter`·볼륨·slice 대조에 쓴다. SSH 단절 → unhealthy, 재접속 시 전체 동기화 반복. 재접속은 지수 백오프(1s 시작, 2배, 최대 30s, ±20% jitter, 성공 시 리셋)로 시도하며 SSH 접속 타임아웃은 10s(코드 상수). local은 "연결 단절"이 없고 runtime 데몬 다운(events 스트림 종료 + `info` 실패)을 unhealthy로 보며, events 재시작에 같은 백오프를 쓴다.
9. 메시지 세션 생성 후 루프 진입. `minRunners`만큼 warm runner는 루프의 desired 계산으로 자연히 배치된다.

### 7.2 루프
1. `GetMessage(maxCapacity = 현재 capacity)`. capacity는 **동적**: unhealthy 머신은 제외되어 다음 폴링부터 유입이 줄어든다.
   메시지 수신·파싱 직후 라이브러리가 `DeleteMessage`로 ack한다(2번 이후의 핸들러 실행 전). 처리 중 크래시로 메시지를 잃어도, acquire된 job은 GitHub 큐에 남고 다음 메시지의 `TotalAssignedJobs`가 desired를 재계산하므로 복구된다(level-triggered).
2. `JobAvailable` 수신 → 용량 안에서만 도착하므로 전부 `AcquireJobs`.
3. desired 계산과 생성:
   ```
   assigned = max(0, TotalAssignedJobs − |pendingCompletion|)
   desired  = min(capacity, max(minRunners, assigned))
   create   = desired − running
   ```
   `running`은 job을 받을 수 있거나 곧 받을 unit 수다: **Creating(기동 중) + Starting(등록 전) + Running + Draining**. Dying은 포함하지 않는다(단, 머신 slot 점유에는 포함. §8.3).

   `pendingCompletion`은 job을 받았던(busy) unit이 정리에 들어갔지만(`die`, 또는 §8.3의 미등록 판정 — job 종료 직후 등록이 `die`보다 먼저 사라지므로 tick이 먼저 볼 수 있다. 어느 경로든 Dying 전이 시점에 넣는다) 그 runner의 `JobCompleted`가 아직 도착하지 않은 runner 이름의 집합이다. listener는 빈 폴링(long-poll 만료)에도 직전 메시지의 `TotalAssignedJobs`를 캐시해 같은 값으로 desired 콜백을 부르므로, 통계가 아직 반영하지 않은 완료분을 이 집합으로 뺀다 — 그래서 빈 폴링에서 유휴 runner가 생기지 않는다. `die`와 `JobCompleted`는 어느 쪽이 먼저 와도 된다: `JobCompleted`가 먼저 오면 unit에 완료 표시만 남기고 그 unit의 `die`는 집합에 넣지 않으며, `die`가 먼저 왔으면 뒤따르는 `JobCompleted`가 집합에서 뺀다(이름 키라 중복 콜백에 멱등). 같은 메시지 안에서는 라이브러리가 `JobCompleted` 핸들러를 desired 콜백보다 먼저 부르므로, 통계가 줄어드는 메시지에서 집합도 함께 비워진다. 안전장치(주 메커니즘이 아니라 안전망): (1) 메시지 세션 재시작 시 집합을 비운다(초기 세션 통계가 새 기준선). 전체 동기화(§7.1-7)에서는 그 머신 소속 unit의 항목을 비운다. (2) 항목은 5분(§8.3 상수 표)이 지나면 버린다. 잔여 실패 모드는 일시적 1개 과소 배치이며 다음 `JobCompleted` 또는 만료에서 자가 치유된다.
   `JobCompleted`의 runner 이름이 비어 있으면 RunnerID로 대조한다. 그 id를 아직 모르는 시점(JIT 결과 수신 전, 입양 unit의 첫 등록 확인 전)에 오면 id를 보관했다가 알게 될 때 적용한다(보관 상한도 5분).
   `create > 0`이면 그 수만큼: spread로 머신 선택 → unit id 발급 → `GenerateJitRunnerConfig(name=<scaleSet>-<machine>-<unit>)` → 컨테이너 실행.
   후보 머신 없음 → pending 유지. job은 GitHub 큐에서 대기(최대 24h). gh-ars는 취소하지 않는다.

   **유휴 runner 축소** (생성과 마찬가지로 통계 메시지에서만 판단):
   ```
   remove = running − desired
   ```
   `remove > 0`이면 그 수만큼 후보를 고른다. 후보는 **Running이고 `JobStarted`로 busy 표시가 없는 unit, 오래된 순**. Creating/Starting/Draining/Dying은 건드리지 않는다(등록 전이라 지워도 소용없고, 곧 등록되면 다음 메시지에서 다시 판단된다).
   후보마다 `RemoveRunner`를 호출하고 unit을 **Draining**으로 둔다. 성공하면 runner 프로세스가 스스로 종료해 `die`가 오고 평소 정리 경로(5번)를 탄다. **컨테이너를 직접 rm하지 않는다.** 실패(job 진행 중을 뜻하는 4xx, `JobStillRunningError` 포함)는 "busy였다"로 보고 Running으로 되돌리고 건너뛴다.
   busy 추적(`JobStarted`의 RunnerName)은 후보를 줄이는 최적화일 뿐이다. job 배정 뒤 `JobStarted` 도착까지 구간이 있으므로 정확성은 `RemoveRunner`의 거절이 보장한다. Draining unit은 tick의 "Running 미등록 → 정리" 판정(§8.3)에서 제외한다.
4. **JIT config 전달** (확정, 유일한 방식): `create` → `cp` → `start` 3단계. attach 의미론에 의존하지 않는다. (`docker run -i`는 ssh 세션 종료 시 SIGHUP이 `--sig-proxy`로 컨테이너에 전달되어 결정적이지 않으므로 쓰지 않는다.)
   ```
   docker create --name gh-ars-<unit>-runner ... --entrypoint /bin/bash <image> -c '<래퍼>'
   <tar 스트림: .jitconfig 1개>  | docker cp - gh-ars-<unit>-runner:/home/runner
   docker start gh-ars-<unit>-runner
   ```
   (podman은 `docker`를 `podman`으로 치환. `podman cp -`도 stdin tar 스트림을 받는다.)

   래퍼는 `jobRuntime.mode`별로 둘이다.
   - none:
     ```
     IFS= read -r ACTIONS_RUNNER_INPUT_JITCONFIG < /home/runner/.jitconfig && rm -f /home/runner/.jitconfig && export ACTIONS_RUNNER_INPUT_JITCONFIG && exec ./run.sh
     ```
   - sidecar (read 앞에 소켓 대기. 30초 상한, 초과 시 비정상 종료 → `die` → 정리 → 재배치. 이 30초는 기동 타임아웃 2분 안에 포함):
     ```
     i=0; until [ -S /var/run/docker.sock ]; do i=$((i+1)); [ $i -ge 150 ] && exit 1; sleep 0.2; done; IFS= read -r ACTIONS_RUNNER_INPUT_JITCONFIG < /home/runner/.jitconfig && rm -f /home/runner/.jitconfig && export ACTIONS_RUNNER_INPUT_JITCONFIG && exec ./run.sh
     ```
     소켓 존재만 확인하는 이유: 커스텀 이미지에 docker CLI가 없을 수 있다. 소켓 생성 뒤 dockerd가 응답하기까지의 잔여 지연은 runner 등록 시간(수 초)이 흡수한다.
   gh-ars가 tar 스트림을 메모리에서 생성해 Executor의 stdin으로 넘기므로 원격 디스크에 파일이 남지 않는다. 래퍼는 파일을 읽어 export하고 즉시 삭제한 뒤 `exec ./run.sh`한다. `ACTIONS_RUNNER_INPUT_JITCONFIG`는 runner의 `--jitconfig` 인자와 동등한 환경변수 입력이다(ARC도 이 방식).
   **요구사항**: 값이 argv, 컨테이너 env(`docker inspect`의 Config.Env·Args), 원격 디스크 어디에도 남지 않으며 컨테이너 안에 `.jitconfig` 파일이 남지 않는다. 프로세스 env(`/proc/<pid>/environ`)는 ARC와 동일하게 허용한다.
   커스텀 이미지도 위 래퍼가 동작해야 한다(bash + `/home/runner/run.sh`, sidecar면 `[ -S ]`를 지원하는 sh, §9.1).
   runner 기동 타임아웃 2분(코드 상수): (sidecar면 slice·볼륨·sidecar 생성 포함) `create`→`cp`→`start`가 2분 안에 끝나 runner 컨테이너가 `running`에 들어가지 못하면 unit 전체 rm 후 재배치. GitHub 등록 대기는 별도로 §8.3의 grace 5분을 따른다.
5. `die` 이벤트 → §8.3 정리 순서대로 unit 정리(GitHub 등록 확인·제거 → 컨테이너 → 볼륨 → slice), 캐시 갱신, `minRunners` 미달분만 보충. busy였던 unit이고 완료 표시가 없으면 runner 이름을 `pendingCompletion`에 넣는다(3번). **신규 unit 생성은 3번의 desired 계산에서만** 일어난다. `die` 시점에 생성하지 않는 것은 특례가 아니라 3번 식의 귀결이다: job 완료 직후 `die`가 먼저 오고 통계는 다음 메시지에서 줄어들지만, `assigned − |pendingCompletion|`이 이미 완료분을 뺀 값이라 캐시된 통계로 계산해도 유휴 runner가 생기지 않는다. pending job도 다음 메시지의 3번에서 처리된다.

### 7.3 종료 (SIGINT/SIGTERM)
- 세션 종료, SSH 연결 정리. **실행 중 컨테이너는 kill하지 않는다**(ephemeral이므로 job 종료 후 자연 소멸, 재시작 시 입양).
- scale set 삭제 없음.

## 8. 용량·배치·Reconcile

### 8.1 용량
```
unit            = scaleSets[].resources          # sidecar 면 runner+sidecar 합산 예산
physicalMax(m)  = floor(min(m.cpu / unit.cpu, m.mem / unit.mem))
effectiveMax(m) = min(physicalMax(m), m.maxRunners?)      # 정수는 하향만 가능
capacity(ss)    = min(ss.maxRunners, Σ effectiveMax(healthy m))
```
capacity가 `X-ScaleSetMaxCapacity`로 보고된다. 적용 방식: none은 컨테이너 1개에 `--cpus`/`--memory`, sidecar는 slice에 합산(§9.3).

### 8.2 spread
- 후보: healthy이고 여유 슬롯 > 0인 머신.
- 기준: `occupied / effectiveMax` **사용률 최저**. (여유 슬롯 절대값 기준이면 큰 머신만 계속 뽑혀 작은 머신이 놀고 큰 머신이 과열된다.)
- `occupied(machine)` = running + Dying(부품 잔존) + Foreign unit. 여유 슬롯(`effectiveMax − occupied`)과 사용률 모두 같은 값을 쓴다. 정리에 실패해 리소스가 실제로 잡혀 있는 머신이 사용률 0으로 보여 과다 배치되지 않게 하기 위함이다(§8.3의 slot 점유 규칙과 같은 이유). `running`(§7.2-3)은 scale set 단위 desired 계산용이고, 머신 단위 배치에는 쓰지 않는다.
- tie-break: ① 여유 슬롯 절대값 큰 쪽 → ② 마지막 배치 시각 오래된 쪽(round-robin 효과) → ③ YAML 순서(결정적).
- warm runner(minRunners)도 같은 규칙.

### 8.3 Reconcile 판정표
"짝이 안 맞음" = 같은 unit id의 부품(runner 컨테이너, sidecar, 볼륨, slice, GitHub 등록)이 완전한 집합이 아닌 상태. **판단 기준은 runner 컨테이너 생존 여부**이지 YAML이 아니다. `ps`는 항상 `-a`.

| 상황 | 조치 |
|---|---|
| runner 컨테이너 살아 있고 라벨의 scale set이 YAML에 없음 | 입양. 예산을 알 수 없으므로 **그 머신의 slot 1개로 센다.** 종료까지 관리하고 새로 띄우지 않음 |
| runner 컨테이너 살아 있고 라벨의 machine 이름이 YAML과 다름 | 입양. 소속(배치·slot 계산)은 SSH로 도달한 현재 머신(또는 local). 단 GitHub 등록 이름은 등록 당시 이름이어야 `GetRunnerByName` 대조가 되므로 라벨의 `gh-ars.scaleSet`/`gh-ars.machine`으로 만든다(§4.3) |
| runner 컨테이너 살아 있고 sidecar/볼륨/slice 일부 없음 | kill하지 않음. `die` 시 나머지 정리 |
| runner 컨테이너 살아 있는데 GitHub에 등록이 없음 (`GetRunnerByName(<scaleSet>-<machine>-<unit>)` 단건 조회, tick 30s마다. **Starting과 Running에 적용, Draining은 제외**) | Starting: 생성 후 grace(5분) 이내면 대기(등록 전 정상 구간). 초과면 unit 정리. Running: grace 없이 즉시 unit 정리(job 종료 직후 등록이 먼저 사라지는 정상 구간이므로 `die`가 곧 뒤따른다). Draining: 축소로 등록을 지운 상태이므로 `die`를 기다린다 |
| Dying unit의 정리 명령 실패 | `Dying` 유지. 머신이 healthy면 매 tick(30s)마다 재시도(별도 백오프 없음). 머신이 unhealthy면 healthy 복귀 후 전체 동기화에서 정리. **부품이 남아 있는 동안 머신 slot을 계속 점유한 것으로 센다**(리소스가 실제로 잡혀 있고, 정리 실패가 과다 배치로 번지지 않게) |
| runner 컨테이너가 `exited` 상태로 남아 있음 | unit 정리 |
| runner 컨테이너 없는 sidecar/볼륨/slice (머신 내부 고아) | 항상 rm. 구현은 부품 집합을 runner 이름 없는 Dying unit으로 등록해 아래 정리 순서(GitHub 단계 생략)·tick 재시도·slot 점유를 그대로 따른다 |
| unit 정리 시 GitHub 등록 처리 (Dying, 기동 타임아웃, 재시작 후 exited 발견 모두 포함) | 컨테이너 rm **전에** `GetRunnerByName`으로 등록을 확인하고, 있으면 `RemoveRunner` 후 rm. `GetRunnerByName`이 미존재(`nil, nil`)를 돌려주거나 `RemoveRunner`가 `RunnerNotFoundError`를 돌려주면(`errors.Is`) 성공으로 취급(ephemeral runner는 보통 스스로 해제 후 종료) |

**Creating 예외**: gh-ars가 지금 만들고 있는 unit(`Creating`)의 부품은 위 표의 판정 대상이 아니다. `create`→`cp`→`start`(§7.2-4)가 진행 중인 동안에는 slice·볼륨·sidecar만 있거나 runner가 `created` 상태인 것이 정상이라, 이를 고아나 짝이 안 맞는 unit으로 보면 자기가 만들던 unit을 지운다. 이 구간의 실패·기동 타임아웃 2분·`die`는 §7.2-4가 책임진다.

**unit 정리 순서**: `GetRunnerByName` → (있으면) `RemoveRunner` → runner 컨테이너 rm → sidecar rm → 볼륨 3개 rm → slice stop + revert.

unit id를 알 수 없는 고아 등록(GenerateJIT 직후·컨테이너 create 전에 gh-ars가 죽은 경우)은 MVP에서 정리하지 않는다(§3.2). 근거(GitHub 문서 "Removing self-hosted runners"): "If JIT runners never run a job, they will automatically be removed", "An ephemeral self-hosted runner is automatically removed from GitHub if it has not connected to GitHub Actions for more than 1 day". 고아 등록은 runner 프로세스가 한 번도 접속하지 않은 JIT·ephemeral 등록이므로 늦어도 1일 안에 GitHub이 지운다. 그동안 offline 상태라 job을 받지 않는다.

상수(MVP에서 코드에 고정, 설정 미노출. 노출은 후속 과제 §3.2).
이 표에는 관측 가능한 동작을 규정하는 상수만 싣는다(상태 전이 트리거, capacity 영향, 타이밍 계약). 관측 가능한 계약을 바꾸지 않는 내부 방어 상수는 DESIGN에 둔다.

| 상수 | 값 | 용도 |
|---|---|---|
| grace | 5분 | 위 표의 등록 대기 |
| 상태 대조 tick | 30s | `GetRunnerByName` 대조, grace·기동 타임아웃 판정 주기 |
| GitHub 큐 대기 | 24h | pending job이 큐에서 기다리는 상한(§7.2-3) |
| 완료 보정 만료 | 5분 | `pendingCompletion` 항목 유지 상한(§7.2-3). tick에서 판정 |
| SSH 접속 타임아웃 | 10s | 접속·재접속 시도 1회당(§7.1-8, §10.1) |
| runner 기동 타임아웃 | 2분 | create→cp→start 완료까지(§7.2-4) |
| 정리 회차 상한 | 2분 | unit 정리(또는 startUnit 되돌리기) 1회 시도의 상한. 초과면 실패로 보고 Dying 유지, 다음 tick(30s)에서 재시도 |
| preflight·관측 probe 상한 | 30s | preflight의 확인 명령(`id -u`, `info`)과 전체 동기화의 관측(`ps`, `volume ls`), events 스트림 **열기** 1회당(스트림 유지에는 상한이 없다). 초과면 그 회차 실패 → 머신 unhealthy → 백오프 재시도(§7.1-3, §7.1-7, §7.1-8) |
| pre-pull 상한 | 5분 | 이미지 pull 1개당(§7.1-4). 초과면 pull 실패 → 그 머신 unhealthy. 이 값이 곧 응답 없는 pull이 세션 시작을 막을 수 있는 최악의 시간이다 |
| SSH keepalive 주기 | 30s | 접속 하나당 생존 확인 주기(§10.1) |
| SSH keepalive 허용 미스 | 3회 | 연속 실패가 이 수에 닿으면 단절로 보고 접속을 닫는다 → unhealthy → 재접속(§10.1). 응답이 오지 않는 프로브는 주기마다 1회 미스로 센다. 조용히 끊긴 접속을 걷어내는 최악의 시간은 `주기 × (미스 + 1)`(120s)다 — 첫 주기는 프로브를 보내는 데 쓰인다 |

## 9. sidecar 모드 상세

### 9.1 구성
- runner 옆에 `--privileged` 데몬 컨테이너 1개. `/var/run` 공유 볼륨(`sock`)의 unix 소켓으로 연결(TCP+TLS 없음, certs 볼륨 없음).
- 엔진은 **scale set에 연결된 머신들의 공통 runtime**(R15)을 따른다:
  - docker → `dockerd` (`docker:29.7.2-dind`)
  - podman → `podman system service`가 같은 소켓 경로에 docker 호환 API를 연다 (`quay.io/podman/stable:v5.8.4`)
- sidecar 이미지는 `scaleSets[].jobRuntime.image`. 생략 시 위 runtime별 기본 이미지. **기본 태그는 코드 상수로 고정하고 릴리스마다 갱신**한다. `:latest`/태그 없음은 오류(R13).
- sidecar 기동 명령:

  | runtime | 이미지 | 명령 | env |
  |---|---|---|---|
  | docker | `docker:29.7.2-dind` | `dockerd --host=unix:///var/run/docker.sock` | `DOCKER_TLS_CERTDIR=""` |
  | podman | `quay.io/podman/stable:v5.8.4` | `podman system service --time=0 unix:///var/run/docker.sock` | – |

  docker에서 `--host`를 명시하는 이유: dind entrypoint는 TLS를 끄면 평문 tcp 2375도 함께 열어 같은 브리지 네트워크의 다른 unit에서 접근할 수 있게 된다. unix 소켓만 연다. 두 경우 모두 `--privileged`, `sock` 볼륨을 `/var/run`에 마운트, `--cgroup-parent=gh-ars-<unit>.slice`.
- 볼륨 3개: `work`(`/home/runner/_work`), `sock`(`/var/run`), `externals`(`/home/runner/externals`). 모두 unit 라벨. runner와 sidecar 양쪽에 마운트.
- runner 환경: `DOCKER_HOST=unix:///var/run/docker.sock` (podman이면 `CONTAINER_HOST`도). workflow의 `docker build`, `container:`, `services:`가 sidecar 안에서 실행된다.
- 공식 runner 이미지(`images/Dockerfile`)는 docker CLI와 buildx 플러그인을 포함하므로 `DOCKER_HOST`가 그대로 동작한다. 커스텀 이미지 사용 시 docker CLI 포함은 **사용자 책임**.
- 커스텀 이미지 공통 요건(모드 무관): gh-ars가 entrypoint를 덮어쓰므로 §7.2-4의 모드별 래퍼가 동작해야 한다. 즉 bash, workdir `/home/runner`와 그 안의 `run.sh`, 그리고 `docker cp`로 `/home/runner/.jitconfig`를 쓸 수 있는 권한(공식 이미지는 `runner` 사용자 소유)을 유지한다. sidecar 모드는 추가로 `[ -S <path> ]`를 지원하는 sh(POSIX `test`)만 있으면 되고 docker CLI는 소켓 대기에 필요하지 않다. 공식 이미지를 `FROM`으로 쓰면 충족된다.
- sidecar는 호스트 root와 동등한 권한을 가진다. CI 전용 머신에만 연결한다. **local 머신을 sidecar scale set에 연결하면 gh-ars 호스트에서 `--privileged` 컨테이너가 뜬다.** 공용 PC·노트북은 none scale set에만 연결한다.

### 9.2 전제 (preflight, R16)
R16은 **환경 모순**만 판정한다: 아래 전제가 확인 결과로 어긋난 경우다. 명령이 실패해 확인 자체를 못 한 경우(데몬 다운, SSH 명령 실패)는 R16이 아니라 unhealthy이며 재접속에서 다시 본다(§7.1-3 분류, §10.2 규칙 2·3).

- rootful docker 또는 rootful podman. rootless면 오류. 판정은 위 `info` 결과에서 한다: podman은 `host.security.rootless`, docker는 `SecurityOptions`에 `name=rootless` 항목이 있는지로 본다(docker에는 전용 필드가 없다).
- cgroup v2 + systemd cgroup 드라이버. preflight는 머신마다 `info --format '{{json .}}'`를 **한 번** 실행해 아래 필드를 읽는다(값만 규범이고 조회는 한 번으로 묶는다):
  - docker: `CgroupDriver` == `systemd`, `CgroupVersion` == `2`
  - podman: `host.cgroupManager` == `systemd`, `host.cgroupVersion` == `v2`(구현이 docker와 맞추어 `2`로 정규화한다). podman이 `--cgroup-parent=<slice>`를 받으려면 systemd 매니저여야 한다
- `systemctl` 사용 가능(systemd 호스트). 정적 분할 fallback 없음.
- §10.2 권한.

### 9.3 slice 생성·정리·값 변환
```
# 생성 (컨테이너 실행 전). slice 유닛은 이름만으로 암묵 생성되며 첫 컨테이너 배치 시 활성화된다.
systemctl set-property --runtime gh-ars-<unit>.slice CPUQuota=<cpu×100>% MemoryMax=<mem>

# 컨테이너 실행 (runner, sidecar 둘 다)
docker run ... --cgroup-parent=gh-ars-<unit>.slice ...     # 개별 --cpus/--memory 없음

# 정리 (die 이후, 컨테이너·볼륨 rm 다음)
systemctl stop gh-ars-<unit>.slice
systemctl revert gh-ars-<unit>.slice                       # set-property 가 만든 runtime drop-in 제거
```
값 변환:

| 설정 | systemd (sidecar) | docker/podman (none) |
|---|---|---|
| `cpu: 2` | `CPUQuota=200%` | `--cpus=2` |
| `cpu: 0.5` | `CPUQuota=50%` | `--cpus=0.5` |
| `cpu: 0.125` | `CPUQuota=12.5%` | `--cpus=0.125` |
| `memory: 4Gi` | `MemoryMax=4G` | `--memory=<bytes>` |
| `memory: 512Mi` | `MemoryMax=512M` | `--memory=<bytes>` |

두 컨테이너가 하나의 예산을 공유하며, 어느 쪽에서 쓰든 이 예산에서 빠진다.

## 10. SSH 및 실행 사용자 정책

### 10.1 SSH
- local 머신은 이 절의 SSH 항목에서 제외된다(권한 요구사항 §10.2는 동일 적용).
- 인증: `keyFile` (+선택 `keyPassphrase`, `${env:}`/`${file:}` 참조 필수)만. host는 hostname/IP 직접 기입.
- host key: `machines[].ssh.fingerprint` → `machineDefaults.ssh.knownHostsFile`(기본 `~/.ssh/known_hosts`) → 둘 다 없으면 거부. TOFU 없음.
- 근거: JIT config(등록 자격 증명)를 SSH로 전달하므로 미검증 호스트 접속을 허용하지 않는다.
- 연결은 머신당 유지(events 스트림 + 명령 실행 채널). **명령 하나의 타임아웃·실패는 그 채널만 끝내고 연결을 끊지 않는다** — 연결을 끊으면 §8.3이 "Dying 유지 + 다음 tick 재시도"로 규정한 국소적 실패가 events 종료·머신 unhealthy·capacity 감소로 번진다. 연결을 끊는 것은 연결 수준 신호(keepalive 연속 실패, 전송 오류)와 프로세스 종료(§7.3)뿐이다.
- 조용히 끊긴(half-open) 연결은 keepalive로 감지한다: 주기적으로 keepalive 요청을 보내고 연속 실패가 허용 미스(§8.3)에 닿으면 단절로 본다. 감지하지 못하면 events는 아무 말도 하지 않고 명령도 매달려, 죽은 머신이 healthy로 남아 계속 배치를 받고 그 unit들은 기동 타임아웃까지 매달렸다 실패하기를 반복한다.
- 단절 시 unhealthy로 전환·경고 로그·capacity에서 제외. 재접속은 지수 백오프(1s 시작, 2배, 최대 30s, ±20% jitter, 성공 시 리셋), 시도 1회당 접속 타임아웃 10s. 재접속 성공 시 전체 동기화(§7.1-7) 후 healthy로 복귀.

### 10.2 SSH 사용자(및 local 실행 사용자) 권한 요구사항
gh-ars는 root가 아닌 사용자로 실행할 때 root가 필요한 명령에만 `sudo -n`을 붙인다. preflight에서 아래 표의 항목을 확인하고 미충족이면 R16 오류(sidecar) 또는 unhealthy(none)로 처리한다.

판단 규칙(preflight, 머신별):
1. `id -u`로 root 여부를 본다.
2. docker: sudo를 쓰지 않는다. `docker info` 실패는 **모드와 무관하게 unhealthy**(도달 불가·명령 실패, §7.1-3 분류). R16은 환경 모순에만 쓴다 — 데몬이 잠시 죽은 것을 R16으로 올리면 재접속 회차에서 그 머신이 `Failed`로 영구 제외된다.
3. podman: `podman info`를 시도하고, 실패하면 `sudo -n podman info`를 시도한다(모드 무관). 성공한 경로(sudo 없음 / `sudo -n`)를 그 머신의 podman 경로로 고정하고 이후 **모든 podman 명령**에 적용한다. 둘 다 실패 → 규칙 2와 같이 모드와 무관하게 unhealthy.
   고정은 **머신 단위로 프로세스 수명 동안** 유지한다: 재접속 preflight는 이 협상을 다시 하지 않고 고정된 경로만 시도하며, 그 경로가 실패하면 다른 경로로 갈아타지 않고 unhealthy로 둔다. rootless와 rootful은 컨테이너 저장소가 달라, 경로가 바뀌면 이미 실행 중인 unit이 보이지 않게 되고 전체 동기화가 그 부품을 "사라졌다"고 판정해 지운다(§8.3).
   sidecar scale set 머신은 추가로: 고정된 경로의 `Rootless=true`이면(rootless podman은 `podman info`가 성공하지만 sidecar에 못 쓴다) 아직 시도하지 않은 `sudo -n podman info`를 시도해 rootful을 얻으면 그 경로로 바꾼다. 그래도 rootful을 못 얻으면 R16 오류.
   none은 어느 경로든 성공하면 healthy(rootless 허용).
4. systemctl: root가 아니면 항상 `sudo -n`.

| runtime | mode | 요구 권한 | preflight 확인 |
|---|---|---|---|
| docker | none | `docker` 그룹 멤버 또는 root | `docker info` 성공(실패는 unhealthy) |
| docker | sidecar | 위 + rootful 데몬 + root 또는 passwordless sudo: `systemctl set-property/stop/revert` (`gh-ars-*.slice` 한정) | `info`의 `SecurityOptions`에 `name=rootless` 없음, `sudo -n systemctl --version` 성공 |
| podman | none | rootless: 일반 사용자. rootful: root 또는 passwordless sudo `podman` | `podman info` 또는 `sudo -n podman info` 중 하나 성공(규칙 3) |
| podman | sidecar | root 또는 passwordless sudo: `podman`, `systemctl set-property/stop/revert` | 규칙 3으로 고정된 경로의 `Rootless=false`, `sudo -n systemctl --version` 성공 |

sudoers 예시(문서화용):
```
runner ALL=(root) NOPASSWD: /usr/bin/systemctl set-property --runtime gh-ars-*.slice *, \
                            /usr/bin/systemctl stop gh-ars-*.slice, \
                            /usr/bin/systemctl revert gh-ars-*.slice, \
                            /usr/bin/podman *
```

## 11. CLI

| 명령 | 동작 |
|---|---|
| `gh-ars run -c <config.yaml> [--log-level debug\|info\|warn\|error] [--log-format json\|text]` | 포그라운드 실행. 검증·preflight·pre-pull·scale set 확보·동기화·루프. 로그는 `log/slog` 기반, 기본 `--log-level info`, `--log-format json`(JSONHandler). `text`는 TextHandler |
| `gh-ars scaleset delete <name> -c <config.yaml>` | GitHub에서 scale set 명시 삭제. 조회 경로는 §7.1-5와 동일(`GetRunnerGroupByName` → `GetRunnerScaleSet(groupID, name)`) → `DeleteRunnerScaleSet(id)`. `runnerGroup`은 설정 파일의 해당 scale set 항목에서 읽고, 설정에 없으면 `Default`. 자동 삭제 없음(이름 == runs-on 라벨이므로 삭제 시 큐 job이 끊기고, YAML에서 뺀 것과 원래 없던 것을 scaler가 구분할 수 없음) |

## 12. 검증 / 수용 기준

`docs/TESTPLAN.md`로 분리했다. §1 유닛 테스트 필수 항목, §2 수동 E2E 체크리스트와 미확정 동작의 대안 표.
