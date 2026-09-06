package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"gh-ars/internal/domain"
	"gh-ars/internal/resource"
)

// maxNamePairLen 은 len(scaleSet)+len(machine) 의 상한이다.
// runner 이름 <scaleSet>-<machine>-<unit> 이 64자를 넘지 않아야 한다. [§4.3, R14]
const maxNamePairLen = domain.MaxRunnerNameLen - domain.UnitIDLen - 2

// resolve 는 치환이 끝난 raw 설정에 기본값을 채우고 정적 규칙을 검증해 Config 를 만든다.
// 오류는 p.errs 에 모으고 계속 진행한다. 오류가 하나라도 있으면 반환값은 쓰지 않는다.
func (p *parser) resolve(raw *rawConfig) *Config {
	cfg := &Config{}
	cfg.GitHub = p.resolveGitHub(raw.GitHub)
	cfg.ScaleSets = p.resolveScaleSets(raw.ScaleSets, cfg.GitHub.Scope)
	cfg.Machines = p.resolveMachines(raw, cfg.ScaleSets)
	p.linkMachines(cfg)
	return cfg
}

// ---------------------------------------------------------------------------
// github
// ---------------------------------------------------------------------------

func (p *parser) resolveGitHub(raw rawGitHub) GitHub {
	gh := GitHub{URL: raw.URL}
	scope, owner, repo, err := parseScope(raw.URL)
	if err != nil {
		p.errs.add("R6", "github.url", err.Error())
	}
	gh.Scope, gh.Owner, gh.Repo = scope, owner, repo

	// [R3] token 과 app 은 택1. app 은 세 필드 모두 필수.
	hasToken := raw.Auth.Token != ""
	hasApp := raw.Auth.App != nil
	switch {
	case hasToken && hasApp:
		p.errs.add("R3", "github.auth", "token 과 app 을 동시에 설정할 수 없음")
	case !hasToken && !hasApp:
		p.errs.add("R3", "github.auth", "token 또는 app 중 하나가 필요함")
	case hasApp:
		a := raw.Auth.App
		var missing []string
		if a.ClientID == "" {
			missing = append(missing, "clientId")
		}
		if a.InstallationID == nil {
			missing = append(missing, "installationId")
		}
		if a.PrivateKey == "" {
			missing = append(missing, "privateKey")
		}
		if len(missing) > 0 {
			p.errs.addf("R3", "github.auth.app", "필수 필드 누락: %s", strings.Join(missing, ", "))
		} else {
			gh.Auth.App = &App{ClientID: a.ClientID, InstallationID: *a.InstallationID, PrivateKey: a.PrivateKey}
		}
	default:
		gh.Auth.Token = raw.Auth.Token
	}
	return gh
}

// parseScope 는 github.url 에서 org/repo 를 판별한다. [R6, §3.1]
// 규칙(구현 기본값, PLAN "남은 결정"): scheme 은 http/https, host 필수, query·fragment·userinfo 없음.
// 경로를 "/" 로 나눈 세그먼트가 1개면 org, 2개면 repo. 첫 세그먼트가 "enterprises" 면
// enterprise scope 라 non-goal(§3.2) 오류. 그 외 개수는 판별 불가. GHES 는 host 만 다르다.
func parseScope(s string) (Scope, string, string, error) {
	if s == "" {
		return "", "", "", errors.New("github.url 이 비어 있음")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", "", "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", "", errors.New("scheme 은 http 또는 https 여야 함")
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", "", errors.New("host 만 있는 순수한 org/repo URL 이어야 함")
	}
	// 앞뒤 슬래시는 하나씩만 벗긴다. Trim 은 "//b" 를 "b" 로 만들어 빈 세그먼트를 숨긴다.
	segs := strings.Split(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/"), "/")
	for _, seg := range segs {
		if seg == "" {
			return "", "", "", errors.New("org 또는 repo 경로를 판별할 수 없음")
		}
	}
	if segs[0] == "enterprises" {
		return "", "", "", errors.New("enterprise scope 는 지원하지 않음 (§3.2)")
	}
	switch len(segs) {
	case 1:
		return ScopeOrg, segs[0], "", nil
	case 2:
		return ScopeRepo, segs[0], segs[1], nil
	}
	return "", "", "", errors.New("org 또는 repo 경로를 판별할 수 없음 (세그먼트가 1개 또는 2개여야 함)")
}

// ---------------------------------------------------------------------------
// scaleSets
// ---------------------------------------------------------------------------

func (p *parser) resolveScaleSets(raws []rawScaleSet, scope Scope) []ScaleSet {
	if len(raws) == 0 {
		p.errs.add("required", "scaleSets", "scale set 이 1개 이상 필요함")
	}
	seen := map[string]bool{}
	out := make([]ScaleSet, 0, len(raws))
	for i, r := range raws {
		path := fmt.Sprintf("scaleSets[%d]", i)
		ss := ScaleSet{Name: r.Name, RunnerGroup: r.RunnerGroup, RunnerImage: r.Runner.Image, SidecarImage: r.JobRuntime.Image}

		if r.Name == "" {
			p.errs.add("required", path+".name", "필수")
		} else if seen[r.Name] {
			p.errs.addf("R8", path+".name", "scale set 이름 중복: %s", r.Name) // [R8]
		}
		seen[r.Name] = true

		if ss.RunnerGroup == "" {
			ss.RunnerGroup = DefaultRunnerGroup
		}
		// [R7] repo scope 는 Default 만. 클라이언트 상수가 "default" 라 대소문자 무시.
		if scope == ScopeRepo && !strings.EqualFold(ss.RunnerGroup, DefaultRunnerGroup) {
			p.errs.addf("R7", path+".runnerGroup", "repo scope 에서는 %s 만 허용: %s", DefaultRunnerGroup, ss.RunnerGroup)
		}

		if r.MinRunners != nil {
			ss.MinRunners = *r.MinRunners
			if ss.MinRunners < 0 {
				p.errs.add("range", path+".minRunners", "0 이상이어야 함")
			}
		}
		// [R23] maxRunners 필수. > Σ effectiveMax 의 cap 은 동적 규칙(plan).
		if r.MaxRunners == nil {
			p.errs.add("R23", path+".maxRunners", "필수")
		} else {
			ss.MaxRunners = *r.MaxRunners
			if ss.MaxRunners < 0 {
				p.errs.add("range", path+".maxRunners", "0 이상이어야 함")
			}
		}

		// [R25]
		if r.Resources == nil {
			p.errs.add("R25", path+".resources", "cpu 와 memory 가 필수")
		} else if b, ok := p.parseBudget(path+".resources", r.Resources); ok {
			ss.Unit = b
		}

		if ss.RunnerImage == "" {
			ss.RunnerImage = DefaultRunnerImage
		}
		p.checkImage(path+".runner.image", ss.RunnerImage)
		if ss.SidecarImage != "" {
			p.checkImage(path+".jobRuntime.image", ss.SidecarImage)
		}

		ss.Mode = domain.Mode(r.JobRuntime.Mode)
		switch ss.Mode {
		case "":
			ss.Mode = domain.ModeNone
		case domain.ModeNone, domain.ModeSidecar:
		default:
			p.errs.addf("mode", path+".jobRuntime.mode", "none 또는 sidecar 여야 함: %s", ss.Mode)
		}
		out = append(out, ss)
	}
	return out
}

// parseBudget 은 resources 블록 하나를 예산으로 만든다. cpu·memory 둘 다 있어야 한다. [R25]
// 두 값을 독립적으로 판정해 오류를 모두 모은다 (DESIGN §8).
func (p *parser) parseBudget(path string, r *rawResources) (resource.Budget, bool) {
	var b resource.Budget
	ok := true
	if r.CPU == nil {
		p.errs.add("R25", path+".cpu", "필수")
		ok = false
	} else if cpu, err := resource.ParseCPU(r.CPU); err != nil {
		p.errs.add("R25", path+".cpu", err.Error())
		ok = false
	} else {
		b.CPU = cpu
	}
	if r.Memory == "" {
		p.errs.add("R25", path+".memory", "필수")
		ok = false
	} else if mem, err := resource.ParseMemory(r.Memory); err != nil {
		p.errs.add("R25", path+".memory", err.Error())
		ok = false
	} else {
		b.MemoryBytes = mem
	}
	if !ok {
		return resource.Budget{}, false
	}
	return b, true
}

// digestPattern 은 OCI 다이제스트 접미(@<알고리즘>:<hex>)다. 등록된 알고리즘 sha256(64자)·sha512(128자)만 받는다.
var digestPattern = regexp.MustCompile(`^@(sha256:[0-9a-f]{64}|sha512:[0-9a-f]{128})$`)

// checkImage 는 :latest 또는 태그 없는 이미지를 거부한다. [R13]
// 유효한 다이제스트(@sha256:...) 참조는 불변이므로 태그와 무관하게 허용한다(구현 기본값, PLAN "남은 결정").
// "@" 뒤가 다이제스트 형식이 아니면 태그 없음과 같이 오류다.
// 태그는 마지막 "/" 뒤의 ":" 로 찾아 레지스트리 포트(localhost:5000/img)와 구분한다.
func (p *parser) checkImage(path, image string) {
	if at := strings.Index(image, "@"); at >= 0 {
		if !digestPattern.MatchString(image[at:]) {
			p.errs.addf("R13", path, "다이제스트 형식이 아님 (@sha256:<64 hex> 또는 @sha512:<128 hex>): %s", image)
		}
		return
	}
	last := image[strings.LastIndex(image, "/")+1:]
	i := strings.LastIndex(last, ":")
	if i < 0 {
		p.errs.addf("R13", path, "태그가 없음: %s", image)
		return
	}
	if tag := last[i+1:]; tag == "latest" || tag == "" {
		p.errs.addf("R13", path, ":latest 또는 빈 태그는 허용하지 않음: %s", image)
	}
}

// ---------------------------------------------------------------------------
// machines
// ---------------------------------------------------------------------------

func (p *parser) resolveMachines(raw *rawConfig, scaleSets []ScaleSet) []Machine {
	if len(raw.Machines) == 0 {
		p.errs.add("required", "machines", "머신이 1개 이상 필요함")
	}
	defRuntime := p.resolveRuntime("machineDefaults.runtime", raw.MachineDefaults.Runtime, domain.RuntimeDocker)
	ssByName := map[string]*ScaleSet{}
	for i := range scaleSets {
		ssByName[scaleSets[i].Name] = &scaleSets[i]
	}

	seen := map[string]bool{}
	localCount := 0
	keys := &keyCache{p: p}
	out := make([]Machine, 0, len(raw.Machines))
	for i, r := range raw.Machines {
		path := fmt.Sprintf("machines[%d]", i)
		m := Machine{Name: r.Name, ScaleSet: r.ScaleSet, Local: r.Host == "", MaxRunners: r.MaxRunners}

		if r.Name == "" {
			p.errs.add("required", path+".name", "필수")
		} else if seen[r.Name] {
			p.errs.addf("R9", path+".name", "머신 이름 중복: %s", r.Name) // [R9]
		}
		seen[r.Name] = true

		if r.ScaleSet == "" {
			p.errs.add("required", path+".scaleSet", "필수")
		} else if _, ok := ssByName[r.ScaleSet]; !ok {
			p.errs.addf("R10", path+".scaleSet", "scale set 이 없음: %s", r.ScaleSet) // [R10]
		} else if r.Name != "" && len(r.ScaleSet)+len(r.Name) > maxNamePairLen {
			// [R14] runner 이름 64자 제한.
			p.errs.addf("R14", path+".name", "len(scaleSet)+len(machine) = %d > %d (runner 이름 %d자 제한)",
				len(r.ScaleSet)+len(r.Name), maxNamePairLen, domain.MaxRunnerNameLen)
		}

		m.Runtime = p.resolveRuntime(path+".runtime", r.Runtime, defRuntime)

		if m.MaxRunners != nil && *m.MaxRunners < 0 {
			p.errs.add("range", path+".maxRunners", "0 이상이어야 함")
		}
		if r.Resources != nil {
			if b, ok := p.parseBudget(path+".resources", r.Resources); ok {
				m.Resources = &b
			}
		}

		if m.Local {
			localCount++
			if r.SSH != nil {
				p.errs.add("R17", path+".ssh", "host 없는 머신(local)에 ssh 블록이 있음") // [R17]
			}
		} else {
			m.SSH = p.resolveSSH(path, r, raw.MachineDefaults.SSH, keys)
		}
		out = append(out, m)
	}
	if localCount > 1 {
		p.errs.addf("R18", "machines", "host 없는 머신(local)은 설정 파일당 1개: %d개", localCount) // [R18]
	}
	return out
}

// resolveRuntime 은 runtime 값을 검증하고 비어 있으면 기본값을 준다. [R12]
func (p *parser) resolveRuntime(path, v string, def domain.RuntimeKind) domain.RuntimeKind {
	switch domain.RuntimeKind(v) {
	case "":
		return def
	case domain.RuntimeDocker, domain.RuntimePodman:
		return domain.RuntimeKind(v)
	}
	p.errs.addf("R12", path, "docker 또는 podman 이어야 함: %s", v)
	return def
}

// resolveSSH 는 machineDefaults.ssh 위에 machines[].ssh 를 덮어 SSH 머신의 접속 설정을 만든다.
// 비어 있는(또는 nil) 머신 값은 "미지정" 이라 기본값을 상속한다. [§6.0, §10.1]
func (p *parser) resolveSSH(path string, r rawMachine, def rawSSH, keys *keyCache) *SSH {
	s := &SSH{Host: r.Host, Port: DefaultSSHPort, User: def.User, KeyFile: def.KeyFile, KeyPassphrase: def.KeyPassphrase}
	knownHosts := DefaultKnownHostsFile
	if def.KnownHostsFile != nil {
		knownHosts = *def.KnownHostsFile
	}
	if def.Port != nil {
		s.Port = *def.Port
	}
	if def.InsecureSkipHostKeyVerify != nil {
		s.InsecureSkipHostKeyVerify = *def.InsecureSkipHostKeyVerify
	}
	portPath, insecurePath := "machineDefaults.ssh.port", "machineDefaults.ssh.insecureSkipHostKeyVerify"
	if r.SSH != nil {
		o := r.SSH.Common
		if o.User != "" {
			s.User = o.User
		}
		if o.Port != nil {
			s.Port = *o.Port
			portPath = path + ".ssh.port"
		}
		if o.KeyFile != "" {
			s.KeyFile = o.KeyFile
		}
		if o.KeyPassphrase != "" {
			s.KeyPassphrase = o.KeyPassphrase
		}
		if o.KnownHostsFile != nil {
			knownHosts = *o.KnownHostsFile
		}
		if o.InsecureSkipHostKeyVerify != nil {
			s.InsecureSkipHostKeyVerify = *o.InsecureSkipHostKeyVerify
			insecurePath = path + ".ssh.insecureSkipHostKeyVerify"
		}
		s.Fingerprint = r.SSH.Fingerprint
	}
	if s.Port <= 0 || s.Port > 65535 {
		p.errs.addf("range", portPath, "1~65535 여야 함: %d", s.Port)
	}
	s.KeyFile = p.expandPath(path+".ssh.keyFile", s.KeyFile)
	s.KnownHostsFile = p.expandPath(path+".ssh.knownHostsFile", knownHosts)

	// [R19] SSH 머신이 있으면 keyFile 필수. passphrase 유무와 키 암호화 여부가 일치해야 한다.
	if s.KeyFile == "" {
		p.errs.add("R19", path+".ssh.keyFile", "SSH 머신에는 keyFile 이 필요함 (machineDefaults.ssh.keyFile 또는 machines[].ssh.keyFile)")
	} else if encrypted, ok := keys.encrypted(path+".ssh.keyFile", s.KeyFile); ok {
		switch {
		case encrypted && s.KeyPassphrase == "":
			p.errs.addf("R19", path+".ssh.keyPassphrase", "키 파일 %s 은 암호화되어 있는데 keyPassphrase 가 없음", s.KeyFile)
		case !encrypted && s.KeyPassphrase != "":
			p.errs.addf("R19", path+".ssh.keyPassphrase", "키 파일 %s 은 암호화되어 있지 않은데 keyPassphrase 가 있음", s.KeyFile)
		}
	}

	// [R20] fingerprint → knownHostsFile → 둘 다 없으면 거부. 우회는 insecureSkipHostKeyVerify 뿐이며 경고.
	// 실제 host key 대조(known_hosts 에 항목이 있는지)는 접속 시 executor/ssh 가 한다.
	if s.InsecureSkipHostKeyVerify {
		p.warnings = append(p.warnings, fmt.Sprintf("R20 %s: host key 검증을 건너뜀 (로컬 테스트 전용). 머신 %s", insecurePath, r.Name))
	} else if s.Fingerprint == "" && s.KnownHostsFile == "" {
		p.errs.add("R20", path+".ssh", "host key 검증 재료가 없음: fingerprint 또는 knownHostsFile 이 필요함")
	}
	return s
}

func (p *parser) expandPath(path, v string) string {
	if v == "" {
		return ""
	}
	out, err := p.expandHome(v)
	if err != nil {
		p.errs.addf("decode", path, "홈 디렉터리 확장 실패: %v", err)
		return v
	}
	return out
}

// keyCache 는 같은 keyFile 을 머신마다 다시 읽지 않는다. [R19]
type keyCache struct {
	p    *parser
	seen map[string]keyResult
}

type keyResult struct {
	encrypted bool
	ok        bool
}

func (k *keyCache) encrypted(path, file string) (bool, bool) {
	if k.seen == nil {
		k.seen = map[string]keyResult{}
	}
	if r, ok := k.seen[file]; ok {
		return r.encrypted, r.ok
	}
	res := keyResult{}
	data, err := k.p.env.ReadFile(file)
	if err != nil {
		k.p.errs.addf("R19", path, "키 파일 %s 읽기 실패: %v", file, err)
	} else if enc, err := keyEncrypted(data); err != nil {
		k.p.errs.addf("R19", path, "키 파일 %s: %v", file, err)
	} else {
		res = keyResult{encrypted: enc, ok: true}
	}
	k.seen[file] = res
	return res.encrypted, res.ok
}

// ---------------------------------------------------------------------------
// scale set ↔ machine 연결
// ---------------------------------------------------------------------------

// linkMachines 는 scale set 마다 연결 머신 목록을 채우고 R11·R15 를 검증하며
// sidecar 기본 이미지를 공통 runtime 으로 확정한다. [R11, R15, §9.1]
func (p *parser) linkMachines(cfg *Config) {
	byName := map[string]int{}
	for i := range cfg.ScaleSets {
		byName[cfg.ScaleSets[i].Name] = i
	}
	runtimes := map[string]map[domain.RuntimeKind]bool{}
	for _, m := range cfg.Machines {
		i, ok := byName[m.ScaleSet]
		if !ok {
			continue // R10 에서 이미 오류
		}
		ss := &cfg.ScaleSets[i]
		ss.Machines = append(ss.Machines, m.Name)
		if runtimes[ss.Name] == nil {
			runtimes[ss.Name] = map[domain.RuntimeKind]bool{}
		}
		runtimes[ss.Name][m.Runtime] = true
	}
	for i := range cfg.ScaleSets {
		ss := &cfg.ScaleSets[i]
		path := fmt.Sprintf("scaleSets[%d]", i)
		if len(ss.Machines) == 0 {
			p.errs.addf("R11", path, "연결된 머신이 없음: %s", ss.Name) // [R11]
			continue
		}
		if ss.Mode != domain.ModeSidecar {
			continue
		}
		kinds := runtimes[ss.Name]
		if len(kinds) != 1 {
			// [R15] sidecar 엔진은 연결 머신의 공통 runtime 을 따르므로 혼합 불가.
			p.errs.addf("R15", path, "sidecar scale set %s 에 연결된 머신의 runtime 이 모두 같지 않음", ss.Name)
			continue
		}
		if ss.SidecarImage == "" {
			for kind := range kinds {
				ss.SidecarImage = DefaultSidecarImage[kind]
			}
		}
	}
}
