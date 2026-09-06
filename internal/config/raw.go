package config

// raw* 는 YAML 파일의 모양을 그대로 옮긴 디코드 전용 구조체다. [§6.0 필드 표]
// 선택 필드 중 "지정 안 함"과 "0 값"을 구분해야 하는 것은 포인터로 받는다
// (maxRunners 의 R23 필수성, insecureSkipHostKeyVerify 의 머신별 override,
// knownHostsFile 의 명시적 빈 값 등).
// `secret:"true"` 태그가 붙은 필드는 R4 검사 대상이다.
// 검증·기본값·병합을 거친 결과는 config.go 의 공개 타입이다.

type rawConfig struct {
	GitHub          rawGitHub          `yaml:"github"`
	ScaleSets       []rawScaleSet      `yaml:"scaleSets"`
	MachineDefaults rawMachineDefaults `yaml:"machineDefaults"`
	Machines        []rawMachine       `yaml:"machines"`
}

type rawGitHub struct {
	URL  string  `yaml:"url"`
	Auth rawAuth `yaml:"auth"`
}

type rawAuth struct {
	Token string  `yaml:"token" secret:"true"`
	App   *rawApp `yaml:"app"`
}

type rawApp struct {
	ClientID       string `yaml:"clientId"`
	InstallationID *int64 `yaml:"installationId"`
	PrivateKey     string `yaml:"privateKey" secret:"true"`
}

type rawScaleSet struct {
	Name        string        `yaml:"name"`
	RunnerGroup string        `yaml:"runnerGroup"`
	MinRunners  *int          `yaml:"minRunners"`
	MaxRunners  *int          `yaml:"maxRunners"`
	Resources   *rawResources `yaml:"resources"`
	Runner      rawRunner     `yaml:"runner"`
	JobRuntime  rawJobRuntime `yaml:"jobRuntime"`
}

// rawResources 의 CPU 는 숫자 또는 문자열이다. yaml.v3 는 int/float64/string 으로 준다. [R25]
type rawResources struct {
	CPU    any    `yaml:"cpu"`
	Memory string `yaml:"memory"`
}

type rawRunner struct {
	Image string `yaml:"image"`
}

type rawJobRuntime struct {
	Mode  string `yaml:"mode"`
	Image string `yaml:"image"`
}

type rawMachineDefaults struct {
	SSH     rawSSH `yaml:"ssh"`
	Runtime string `yaml:"runtime"`
}

// rawSSH 는 machineDefaults.ssh 와 machines[].ssh 의 공통 필드다. [§6.0]
type rawSSH struct {
	User                      string  `yaml:"user"`
	Port                      *int    `yaml:"port"`
	KeyFile                   string  `yaml:"keyFile"`
	KeyPassphrase             string  `yaml:"keyPassphrase" secret:"true"`
	KnownHostsFile            *string `yaml:"knownHostsFile"`
	InsecureSkipHostKeyVerify *bool   `yaml:"insecureSkipHostKeyVerify"`
}

// rawMachineSSH 는 machines[].ssh 다. fingerprint 는 머신에만 있다. [§6.0, R20]
type rawMachineSSH struct {
	// Common 은 이름 있는 필드다. 임베딩하면 unexported 타입이라 reflect 로 값을 쓸 수 없다(subst.go).
	Common      rawSSH `yaml:",inline"`
	Fingerprint string `yaml:"fingerprint"`
}

type rawMachine struct {
	Name       string         `yaml:"name"`
	ScaleSet   string         `yaml:"scaleSet"`
	Host       string         `yaml:"host"`
	SSH        *rawMachineSSH `yaml:"ssh"`
	Runtime    string         `yaml:"runtime"`
	MaxRunners *int           `yaml:"maxRunners"`
	Resources  *rawResources  `yaml:"resources"`
}
