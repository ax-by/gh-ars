# DECISIONS.md — 설계 결정 기록

역할: 왜 그렇게 정했는가. 각 행의 `[§n]`은 SPEC.md 참조. 새 결정은 아래 표에 행을 추가한다(결정 / 이유 / 기각한 대안).

| 결정 | 이유 | 대안 |
|---|---|---|
| listener 패키지 사용 | 세션·폴링·ack·acquire를 검증된 구현에 맡김. 우리는 Scaler 3개 메서드만 구현 | MessageSessionClient 직접 사용 (제어는 늘지만 코드·버그 증가) |
| 단일 Controller goroutine | 상태 경합 제거, 테스트에서 메시지 시퀀스로 재현 가능 | 머신별 락 |
| unit 생성/정리는 goroutine + 결과 메시지 | 느린 SSH/pull이 루프를 막지 않음 | 루프 안에서 동기 실행 |
| Runtime이 Executor 위에서 CLI 호출 | thin control 원칙(원격 에이전트 없음), docker/podman 차이를 argv 수준에서 흡수 | Docker SDK를 SSH 터널로 연결 (podman 호환·권한 처리 복잡) |
| yaml.v3 KnownFields | strict decode를 표준 라이브러리 수준에서 해결 | goccy/go-yaml |
| GitHub 등록 제거를 컨테이너 rm **앞에** 둔다 [§8.3] | runner 목록 API가 없어 컨테이너가 사라지면 등록을 찾을 방법이 없다. 이름을 아는 동안 제거한다. 놓치더라도 GitHub이 ephemeral 등록을 1일 미접속 시 자동 제거하므로 피해는 `TotalRegisteredRunners`가 잠시 부풀 정도로 한정된다 | REST runners 목록(App 설치 토큰 발급을 별도 구현해야 함) / GitHub 자동 제거에만 의존 |
| die 시 신규 unit을 만들지 않는다 [§7.2-6] | die가 통계 갱신보다 먼저 오므로 캐시 값으로 재생성하면 유휴 runner가 생긴다. minRunners 보충만 | die 시 즉시 재생성 |
| scale set 그룹 이동 분기 삭제 [§7.1-5] | `GetRunnerScaleSet`이 그룹 단위 조회라 다른 그룹의 동명 scale set을 발견할 수 없어 분기가 도달 불가 | 전 그룹 순회(그룹 목록 API 필요, MVP 범위 밖) |
| dind는 unix 소켓만 개방 [§9.1] | TLS를 끄면 entrypoint가 tcp 2375도 열어 같은 브리지의 다른 unit에서 접근 가능 | 기본 entrypoint 사용 |
| secret 치환은 struct 필드 단위 [§6.0] | `${file:}`로 들어오는 여러 줄 PEM이 원문 치환에서 YAML을 깨뜨림 | 원문 텍스트 치환 |
| 유휴 runner 축소는 `RemoveRunner`의 거절에 정확성을 맡긴다 [§7.2-3] | job 배정 뒤 `JobStarted` 도착까지 구간이 있어 busy 추적만 믿으면 막 job을 받은 runner를 지울 수 있다. busy 추적은 후보를 줄이는 최적화 | 축소 없음(scale-to-zero 동기와 충돌) / busy 추적만으로 판단 |
| 축소 대상은 `Draining`으로 두고 컨테이너를 직접 rm하지 않는다 [§7.2-3] | 등록 삭제 후 runner 프로세스가 스스로 종료해 평소 die 경로를 탄다. tick의 미등록 판정과 충돌하지 않게 상태로 구분 | RemoveRunner 후 즉시 rm |
| R24는 도달한 머신 기준, 미도달 시 경고 [§6.2] | desired가 `min(capacity, …)`라 위반해도 동작이 깨지지 않는다. 일시 단절로 시작 실패시키지 않음 | 항상 오류 |
| 재접속 후 R16/R21 위반은 머신 `Failed` [§7.1-3] | 프로세스 시작 실패가 불가능한 시점. 재접속 대상에서도 빼서 반복 오류 방지 | unhealthy 유지(무한 재시도) |
| podman 경로는 `info` 성공 경로로 고정, 모드 무관 sudo 재시도 [§10.2] | rootful podman을 sudo로 쓰는 none 머신을 unhealthy로 만들지 않기 위해. sidecar만 rootful 조건을 추가 확인 | none은 sudo 미시도 |
| sidecar 소켓 대기는 래퍼에 둔다 [§7.2-4] | gh-ars가 SSH로 폴링하면 왕복이 많고 local/ssh 구현이 갈라진다. `[ -S ]`만 써서 docker CLI 없는 커스텀 이미지도 지원 | Controller 폴링 |
| GitHub 등록 대조는 `msgTick`에서만, `Reconcile`은 부품 집합만 판정. 입양된 살아 있는 unit은 등록 여부를 보지 않고 `Starting`으로 진입 [§8.3, DESIGN §5] | 같은 판정표 행을 두 곳에서 구현하면 중복·누락이 생긴다. Reconcile에서 GitHub을 빼면 순수 함수로 남고, 입양 후 첫 tick(≤30s)이 Running/Dying을 결정하므로 결과는 같다 | Reconcile이 등록 여부 map을 입력으로 받아 해당 행도 판정 |
| 구현 루프 게이트를 PowerShell 스크립트(`gate.ps1`/`commit.ps1`)와 `.loop/state.json`으로 둔다 | 테스트·리뷰 통과 여부와 회차는 결정론적으로 판정할 수 있고, 파일에 남겨야 세션이 끊겨도 진행 상황이 보존된다. 개발 머신이 Windows이고 리포 스크립트를 PowerShell로 통일했다. 훅은 exec 형식으로 `powershell.exe`를 직접 실행해 셸 의존이 없다 | 에이전트 기억에 의존 / bash 스크립트 / Makefile |
| 게이트 스크립트는 PLAN.md를 파싱하지 않고 단계 정보를 인자로 받는다 | 마크다운 표 파싱은 문서 편집 한 번에 깨지고, 깨지면 게이트가 조용히 잘못된 범위를 검사한다. 인자는 명시적이고 state.json에 그대로 남는다. 상태 칸 갱신만 예외로 두되 실패해도 경고만 한다 | PLAN.md 표를 단일 출처로 파싱 |
| 리뷰 출력에서 Verdict를 못 찾거나 여러 개면 BLOCK으로 처리한다 | 리뷰어가 출력 계약을 어겼을 때 PASS로 새어 나가면 게이트가 무의미하다. 안전한 쪽(fail closed)으로 실패시키고 원문을 `.codex-review/last.md`에 남겨 사람이 판단한다 | 마지막 PASS/BLOCK 단어를 휴리스틱으로 추출 |
| Dying 정리 실패는 tick 재시도 + slot 점유 유지 [§8.3] | 리소스가 실제로 잡혀 있으므로 slot을 해제하면 과다 배치. 30s tick이 재시도 간격 | 별도 백오프 / 즉시 slot 해제 |
| slice CPUQuota 를 정수 퍼센트가 아닌 문자열로 넘기고, cpu 하한을 R25 검증 오류로 둔다 [§9.3, R25] | DESIGN §3.4(`SystemdProps` → `"200%"`)와 §4.3(`Create(..., cpuQuotaPercent int, ...)`)은 정수가 아닌 cpu 에서 양립하지 않았다. int 를 경유하면 `cpu: 0.125` 가 `12%` 로 잘려 none 모드(`--cpus=0.125`)와 sidecar 모드의 예산이 달라지고 §8.1 의 "unit 예산 하나" 전제가 깨진다. 하한 아래 값을 조용히 클램프하는 대신 시작 시 오류로 떨어뜨려 규범을 SPEC 에 남긴다 | 정수 퍼센트 + 1% 클램프(적용값이 사용자 값과 달라짐, 근거가 SPEC 밖에 남음) |
| R19 의 키 파일 암호화 판정을 PEM 머리 파싱으로 한다(openssh-key-v1 의 ciphername/kdfname, `Proc-Type: 4,ENCRYPTED`, `ENCRYPTED PRIVATE KEY`) [R19] | R19 는 정적 규칙이라 config 가 판정한다(§7.1-1). `golang.org/x/crypto/ssh` 는 Phase 8 도입(DESIGN §10)이고, 판정 조건은 `ssh.ParseRawPrivateKey` 가 `PassphraseMissingError` 를 내는 조건과 같아 결과가 달라지지 않는다 | x/crypto 를 Phase 2 로 앞당김(쓰지 않는 SSH 코드가 먼저 들어옴) / R19 를 executor/ssh 로 미룸(§7.1-1 의 정적 규칙 목록과 충돌) |
