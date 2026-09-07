package executor

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// [§10.2] Sudo 는 root 가 필요한 명령에만 `sudo -n` 접두를 붙인다.
// 붙일지 말지는 preflight 의 판단이고 Executor 는 플래그를 그대로 따른다.
func TestResolveArgv_S10_2_Sudo(t *testing.T) {
	tests := []struct {
		name string
		in   Cmd
		want []string
	}{
		{"sudo 없음", Cmd{Argv: []string{"docker", "info"}}, []string{"docker", "info"}},
		{"sudo 있음", Cmd{Argv: []string{"podman", "info"}, Sudo: true}, []string{"sudo", "-n", "podman", "info"}},
		{
			"systemctl set-property",
			Cmd{Argv: []string{"systemctl", "set-property", "--runtime", "gh-ars-x.slice", "CPUQuota=200%"}, Sudo: true},
			[]string{"sudo", "-n", "systemctl", "set-property", "--runtime", "gh-ars-x.slice", "CPUQuota=200%"},
		},
		{"인자 없는 단일 명령", Cmd{Argv: []string{"id"}, Sudo: true}, []string{"sudo", "-n", "id"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveArgv(tc.in)
			if err != nil {
				t.Fatalf("resolveArgv: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("resolveArgv = %v, want %v", got, tc.want)
			}
		})
	}
}

// [§10.2] 접두를 붙이면서 호출자의 Argv 를 건드리면 같은 Cmd 를 재사용하는
// 호출자(재접속 후 preflight 재실행)가 `sudo -n` 을 중복으로 얻는다.
func TestResolveArgv_S10_2_DoesNotMutateInput(t *testing.T) {
	argv := []string{"podman", "info"}
	c := Cmd{Argv: argv, Sudo: true}
	if _, err := resolveArgv(c); err != nil {
		t.Fatalf("resolveArgv: %v", err)
	}
	if _, err := resolveArgv(c); err != nil {
		t.Fatalf("resolveArgv 2회차: %v", err)
	}
	if !reflect.DeepEqual(argv, []string{"podman", "info"}) {
		t.Fatalf("입력 Argv 가 변했다: %v", argv)
	}
}

// [§5] 빈 명령은 실행하지 않는다.
func TestResolveArgv_S5_EmptyArgv(t *testing.T) {
	for _, c := range []Cmd{{}, {Argv: []string{}}, {Argv: nil, Sudo: true}} {
		if _, err := resolveArgv(c); !errors.Is(err, errEmptyArgv) {
			t.Fatalf("resolveArgv(%v) err = %v, want errEmptyArgv", c.Argv, err)
		}
	}
}

// [§7.2-4] 명령 실패 로그에는 argv 와 stderr 가 함께 남아야 한다.
// JIT config 는 stdin 으로만 전달되므로 argv 에 secret 이 없다.
func TestCmdError_S7_2_4_Message(t *testing.T) {
	e := &cmdError{argv: []string{"docker", "events"}, exitCode: 125, stderr: "  boom\n"}
	got := e.Error()
	for _, want := range []string{"docker events", "125", "boom"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Error() = %q, %q 가 없다", got, want)
		}
	}
	// stderr 가 비면 콜론만 남는 꼬리를 붙이지 않는다.
	if got := (&cmdError{argv: []string{"docker"}, exitCode: 1}).Error(); strings.HasSuffix(got, ":") {
		t.Fatalf("Error() = %q, 빈 stderr 에 꼬리가 붙었다", got)
	}
}
