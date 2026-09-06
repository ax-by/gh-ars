package config

import (
	"errors"
	"fmt"
)

// RuleError 는 검증 규칙 하나의 위반이다. Rule 은 SPEC §6.2 의 번호("R13") 또는
// 번호 없는 정적 검사의 이름("required", "range", "mode", "decode")이다. [§6.2]
// Path 는 YAML 경로("machines[1].ssh.keyFile")이며 전역 규칙(R18 등)은 비어 있다.
// Msg 에 secret 값을 넣지 않는다.
type RuleError struct {
	Rule string
	Path string
	Msg  string
}

func (e *RuleError) Error() string {
	if e.Path == "" {
		return e.Rule + ": " + e.Msg
	}
	return e.Rule + " " + e.Path + ": " + e.Msg
}

// Rules 는 errors.Join 으로 묶인 오류에서 위반 규칙 이름을 순서대로 뽑는다. 테스트와 로그용.
func Rules(err error) []string {
	var out []string
	var walk func(error)
	walk = func(err error) {
		switch e := err.(type) {
		case nil:
		case *RuleError:
			out = append(out, e.Rule)
		case interface{ Unwrap() []error }:
			for _, c := range e.Unwrap() {
				walk(c)
			}
		case interface{ Unwrap() error }:
			walk(e.Unwrap())
		}
	}
	walk(err)
	return out
}

// errs 는 검증 중 발견한 위반을 모은다. 전부 모아 한 번에 보고한다 (DESIGN §8).
type errs struct{ list []error }

func (e *errs) add(rule, path, msg string) {
	e.list = append(e.list, &RuleError{Rule: rule, Path: path, Msg: msg})
}

func (e *errs) addf(rule, path, format string, a ...any) {
	e.add(rule, path, fmt.Sprintf(format, a...))
}

func (e *errs) join() error { return errors.Join(e.list...) }
