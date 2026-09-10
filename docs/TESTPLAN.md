# TESTPLAN.md — 검증 / 수용 기준

역할: SPEC.md의 규범을 어떻게 검증하는가. 유닛 테스트 필수 항목과 사람이 수행하는 E2E 체크리스트. 규칙 번호(R2~R25)와 절 번호(§n)는 SPEC.md를 가리킨다. E2E 결과는 `PLAN.md`에 기록한다.


## 1. 유닛 테스트 (필수)

각 항목 앞의 괄호가 소속 태그다. PLAN.md Phase의 완료 판정은 "그 Phase에 배정된 태그가 붙은 항목 전부 통과"로 한다. 태그 → Phase 대응은 PLAN.md Phase 표의 완료 기준 칸에 있다.

- (resource) `Ki/Mi/Gi` 파싱, 숫자·문자열 cpu 파싱, 음수·0·비숫자 거부 (R25).
- (resource) systemd 값 변환(cpu → CPUQuota, memory → MemoryMax) 및 docker 플래그 변환 (§9.3 표).
- (domain) runner 이름 `<scaleSet>-<machine>-<unit>` 조립, 컨테이너·볼륨·slice 이름, 라벨 세트, 이름에서 unit/role 파싱 (§4.2, §4.3).
- (config) 설정 검증 규칙 R2~R25 전부(strict decode 포함). 정적 규칙은 config에서, 동적 규칙(R16, R21, R22, R23 cap, R24)은 각 소속 패키지에서.
- (config) `${env:}` / `${file:}` 치환(struct 필드 단위), secret 리터럴 거부 (R4, R5).
- (config) org/repo URL 판별, repo scope `runnerGroup` 제약 (R6, R7).
- (config) runner 이름 길이 규칙 (R14).
- (plan) 용량 계산: physicalMax / effectiveMax / capacity, cap 경고 (R22, R23).
- (plan) desired 공식: minRunners·TotalAssignedJobs·capacity 조합, 기동 중 컨테이너 포함 (§7.2-3).
- (plan) 축소 후보 선정: Running·non-busy·오래된 순, Creating/Starting/Draining 제외, remove 개 초과 금지 (§7.2-3).
- (plan) spread 배치와 tie-break: 동률 시 결정적 순서, Foreign·Dying의 slot 점유 반영 (§8.2).
- (plan) reconcile 판정표 §8.3 중 부품 집합으로 판정하는 행 전부: 입양(scale set 없음·machine 다름), 부품 일부 없음, exited, 머신 내부 고아. 등록 관련 행은 (controller).
- (executor/ssh) host key 검증 순서(fingerprint → known_hosts → 거부), `insecureSkipHostKeyVerify` 경고 (R20).
- (executor/ssh) host key 알고리즘 목록은 라이브러리 지원 목록의 재정렬이고(ed25519 계열 우선, 인증서 알고리즘 포함, `ssh-rsa` 미포함), known_hosts 재시도 목록은 파일이 가진 키의 **계열 전체**로 펼쳐진다 (§10.1, R20).
- (executor/ssh) fingerprint 경로는 계열을 차례로 시도한다: 첫 계열의 실패(지문 불일치·해당 계열 키 없음)가 다음 계열 시도를 막지 않는다 (§10.1, R20).
- (executor/ssh) 명령 하나의 ctx 만료가 접속을 끊지 않는다: 같은 접속의 events 스트림과 이후 명령이 살아 있다(in-process SSH 서버로 검증) (§10.1, DESIGN §4.1).
- (executor/ssh) keepalive 판정(상태 기계): 실패는 miss 증가, 성공은 리셋, **응답 없는 프로브는 주기마다 miss**, 상한 도달 시 단절. stop 시 루프 종료 (§10.1, §8.3).
- (executor/ssh) known_hosts 재시도 목록에서 RSA 항목은 `rsa-sha2-256/512`로 펼친다(OpenSSH 8.8+ 는 SHA-1 서명을 끈다) (§10.1, R20).
- (executor/local) `Cmd.Sudo` 접두, stdin 전달, Stream 종료 통지 (§5).
- (runtime/docker), (runtime/podman) `ImageExists`: `image inspect` argv(podman 은 고정된 sudo 경로 전파), "이미 없음" 응답만 false, 그 밖의 실패(데몬 접속 불가 등)는 오류 (§7.1-4, DESIGN §4.2).
- (runtime/docker) `Info`의 rootless 판정(`SecurityOptions`의 `name=rootless`), docker argv 조립과 출력 파싱: ps -a 라벨 필터, none create 플래그(--cpus/--memory, 라벨, entrypoint 래퍼), events JSON 정규화, `Info` 파싱 (DESIGN §4.2).
- (runtime/docker), (runtime/podman) 읽지 못한 줄 판정: 깨진 JSON·`type != container`·이름 없음은 전부 이상 신호이며, 첫 줄은 즉시 경고 로그, 집계는 스트림 종료 오류에 실린다 (DESIGN §4.2).
- (runtime/podman) podman과 docker의 차이: events JSON 필드(**픽스처는 실제 podman 출력을 캡처한 것이어야 한다** — 지어낸 스키마는 파서가 모든 줄을 버려도 통과한다), 시각 필드의 정수·문자열 양쪽 수용, 읽지 못한 줄이 스트림 종료 오류에 실리는지, `Info`의 `v2` → `2`·`Rootless`, `Cmd.Sudo` 접두 전파 (DESIGN §4.2, §10.2 규칙 3).
- (runtime/sidecar) sidecar create 플래그(--cgroup-parent, --privileged, 볼륨 3개 마운트 경로, dind/podman 명령·env)와 runner 컨테이너의 `DOCKER_HOST`/`CONTAINER_HOST` (§9.1, DESIGN §4.2 표).
- (runtime/jittar) `.jitconfig` 1개짜리 tar 스트림 생성(경로·mode 0600·uid/gid 1001)과 `cp -` 대상 경로 `/home/runner`, 모드별 래퍼 문자열이 SPEC §7.2-4와 일치 (§7.2-4).
- (systemd) set-property/stop/revert argv, `Check`의 sudo 분기 (§9.3, §10.2).
- (github) `CheckAuth`가 그룹 조회로 인증을 확인하고(`Default` 정규화 포함) 실패를 그대로 올린다 (§7.1-2, R7).
- (github) `GetRunner`의 `(nil, nil)` → found=false, `RemoveRunner`의 `RunnerNotFoundError` → nil, `IsBusy` 판정, `Default` 대소문자 무시 (§8.3, R7).
- (machine) podman 경로 고정 규칙(§10.2 규칙 3)의 none/sidecar 분기, `id -u` 분기, R16 판정(rootless docker·podman, cgroup, systemctl).
- (machine) R21 위반의 등급은 **그 머신을 처음 판정하는가**로 갈린다: 최초 판정(시작 시 미도달이었다가 재접속 후 드러난 경우 포함) → Failed(재접속 중단), 이미 통과했던 머신의 재판정 → Failed 도 Unhealthy 도 아니고 회차가 그대로 Resynced 까지 간다 (§7.1 재접속 회차, R21).
- (machine) pre-pull 의 자리는 회차 종류로 갈린다: 최초 접속 회차는 events·동기화 **앞**, 재접속 회차는 Resynced **뒤**. pull 실패는 그 이미지가 이미 머신에 있으면(`ImageExists`) 경고로 낮추고 성공, 없으면 그대로 실패 (§7.1-4, §7.1 재접속 회차, R13).
- (machine) 취소 사유 규약: 여러 단계를 덮는 회차 ctx 를 마감 타이머가 취소하면 뒤따르는 단계의 `context canceled` 가 아니라 취소 사유를 보고한다. 사유 없는 취소(부모 ctx 종료)는 치환하지 않는다 (DESIGN §1-5, §7.1-8).
- (machine) 재접속 백오프 수열(1s→2s→…→30s cap, jitter 범위, 성공 시 리셋) (§7.1-8).
- (controller/core) 시작 순서: 인증 확인(§7.1-2)이 preflight·pre-pull보다 앞. 인증 실패면 pull·scale set 확보 없이 시작 실패.
- (controller/core) 시작 preflight는 머신별 병렬이고, 결과 적용·로그는 설정 머신 순서다. 설정·환경 모순이 여러 머신에서 나오면 첫 건에서 멈추지 않고 전부 보고한 뒤 시작 실패 (§7.1-3).
- (controller/core) 시작 preflight에 실패한 머신은 세션 시작을 기다리게 하지 않는다(그 머신의 첫 통지 없이도 세션이 열린다). 기다리면 그 머신의 첫 회차가 pre-pull을 다시 돌아 §8.3의 pre-pull 상한이 두 번 얹힌다 (§7.1-4, §7.1-9).
- (controller/core) 등록 대조 회차는 한 번에 하나다: 도는 중의 tick은 새 회차를 띄우지 않고(unit 하나당 호출 1회) 경고만 남기며, 회차가 끝나면 다음 tick이 정상적으로 새 회차를 띄운다 (§8.3 "상태 대조 tick", DESIGN §4.4).
- (controller/core) `msgDesired` → desired 계산 → spread → `startUnit`(create→cp→start) → `Starting`, `SetMaxRunners` 반영 (§7.2-1·3·4).
- (controller/core) tick의 등록 대조: Starting 승격·grace 초과 Dying, Running 미등록 즉시 Dying (§8.3).
- (controller/core) 정리 순서: GetRunner → RemoveRunner → 컨테이너 → sidecar → 볼륨 → slice, 각 단계 멱등 (§8.3).
- (controller/core) die 후 신규 생성 없음(minRunners 보충만), Creating 중 die, startUnit 실패 역순 정리 (§7.2-5).
- (controller/ext) R24의 도달/미도달 분기 (시작 시 1회).
- (controller/ext) 축소: `msgDesired`에서만, `RemoveRunner` 거절 시 Running 복귀, Draining 중 die → Dying, Draining은 tick 대조 제외 (§7.2-3).
- (controller/ext) `pendingCompletion` 보정: busy unit die → 캐시 값 `msgDesired`에서 생성 없음, `JobCompleted`가 die보다 먼저·나중 어느 쪽이든 항목이 남지 않음, 같은 메시지의 `JobCompleted`+`JobAvailable`(값 동일)에서 생성됨, 세션 재시작·재동기화·5분 만료로 비워짐 (§7.2-3).
- (controller/ext) 재동기화의 순서: `pendingCompletion` 비우기가 `Reconcile` 적용보다 **앞**이다. 단절 중 죽은 busy·미완료 unit이 그 재동기화에서 Dying이 되면 `markDying`이 넣은 항목이 처리 후에도 남고, 단절 전에 쌓인 낡은 항목은 비워진다 (§7.2-3, DESIGN §4.5·§6).
- (controller/ext) `msgResynced` → `plan.Reconcile` 결과 적용: 입양(Starting 진입), RemoveUnit, RemoveOrphan (§8.3, DESIGN §5 경계).
- (controller/ext) 재동기화 후 known unit의 `Parts`가 스냅샷과 일치하고 `State`·`Busy`는 보존된다: 캐시에 있던 부품이 스냅샷에 없으면 지워지고, 스냅샷에만 있으면 채워진다 (DESIGN §6 `msgResynced`).
- (controller/ext) Dying unit의 slot 점유 유지와 tick 재시도, unhealthy 머신의 Dying 보류, `msgHealth`에 따른 capacity 재계산 (§8.3).

## 2. 수동 E2E 체크리스트 (실제 GitHub **repo scope**, 결과는 로그 + `docker ps` + GitHub runner 목록 스크린샷으로 기록)
1. **none 모드**: scale set 1개에 local 머신 1대 + docker 머신 1대 + podman 머신 1대. `runs-on: <scaleSet>` job 3개 동시 push → 3 unit이 spread로 3대에 배치되어 완료 → 컨테이너와 GitHub runner 등록이 모두 사라짐. 실행 중 `docker inspect`의 Config.Env·Args에 JIT 값이 없고, 컨테이너 안에 `/home/runner/.jitconfig`가 남지 않음(`docker exec <ctr> test ! -e /home/runner/.jitconfig`). 정리 시 `GetRunnerByName` → `RemoveRunner`(또는 미존재) → 컨테이너 rm 순서가 로그로 확인됨.
2. **sidecar 모드**: docker용 `linux-x64-docker`(rootful docker 머신)와 podman용 `linux-x64-podman`(rootful podman 머신) scale set 2개. 각각에서 `docker build`, `container:` job, `services:`를 쓰는 job이 성공. runner+sidecar가 같은 `gh-ars-<unit>.slice` 아래 실행됨을 확인. 종료 후 두 컨테이너·볼륨 3개·slice 모두 정리됨.
3. job 실행 중 프로세스 재시작 → 실행 중 unit을 입양하고 **중복 생성 없음**.
4. 머신 1대 SSH 단절 → 해당 머신 배치 제외 + 경고 로그 + capacity 감소 보고.
5. `gh-ars scaleset delete <name>` → GitHub에 scale set이 남지 않음.

E2E에서 검증하는 미확정 동작(실패 시 대안을 적용하고 이 문서를 갱신):

| 동작 | 검증 | 실패 시 대안 |
|---|---|---|
| 활성화 전 slice에 대한 `systemctl set-property --runtime` | E2E 2에서 `systemctl show gh-ars-<unit>.slice -p CPUQuotaPerSecUSec,MemoryMax`가 설정값을 보임 | `systemd-run --slice=gh-ars-<unit>.slice --unit=gh-ars-<unit>-anchor sleep infinity`로 선활성화 후 set-property. 정리 시 anchor도 stop |
| `podman cp -`의 stdin tar 수신 | E2E 1의 podman 머신에서 `.jitconfig` 주입이 성공하고 runner가 등록됨 | 원격 임시 파일(`mktemp`, 0600) 경유 후 즉시 삭제. 이 경우 §7.2-4 "원격 디스크에 남지 않음"을 "즉시 삭제"로 완화하고 기록 |

org scope는 URL 파싱·scope 판별·runnerGroup 제약 유닛 테스트로 대체(등록·수신·생성·정리 경로는 repo와 동일). 가짜 Scale Set 서버와 자동화 통합 테스트는 비용 대비 가치가 낮아 이후로 미룬다.

