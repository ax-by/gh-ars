package config

import (
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
)

// refPattern 은 값 전체가 참조 하나인 경우만 인정한다. 부분 삽입("x-${env:A}")은 참조가 아니라
// 리터럴이다(secret 필드면 R4, 그 외는 그대로 둔다). [§6.0, R4]
var refPattern = regexp.MustCompile(`^\$\{(env|file):([^}]+)\}$`)

// substitute 는 디코드된 raw 구조체의 모든 string 필드를 순회하며
// `${env:NAME}` / `${file:PATH}` 참조를 값으로 바꾼다. 원문 텍스트 치환은 하지 않는다:
// `${file:}` 로 들어오는 여러 줄 PEM 이 YAML 구조를 깨뜨리기 때문이다. [§6.0, DESIGN §8]
// `secret:"true"` 필드에 참조가 아닌 값이 있으면 R4, 참조 대상이 없으면 R5 다.
// cpu 의 any 필드는 숫자 입력이므로 순회하지 않는다.
func (p *parser) substitute(v reflect.Value, path string, secret bool) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			p.substitute(v.Elem(), path, secret)
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, inline := yamlName(f)
			child := path
			if !inline {
				child = joinPath(path, name)
			}
			p.substitute(v.Field(i), child, secret || f.Tag.Get("secret") == "true")
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			p.substitute(v.Index(i), fmt.Sprintf("%s[%d]", path, i), secret)
		}
	case reflect.Interface:
		// cpu 의 any 필드. 문자열이면 다른 문자열 필드와 같이 치환하고 숫자는 그대로 둔다. [§6.0, R25]
		if !v.IsNil() && v.Elem().Kind() == reflect.String {
			if val, ok := p.substituteString(v.Elem().String(), path, secret); ok {
				v.Set(reflect.ValueOf(val))
			}
		}
	case reflect.String:
		if val, ok := p.substituteString(v.String(), path, secret); ok {
			v.SetString(val)
		}
	}
}

// substituteString 은 값 하나를 판정한다. 치환된 값이 있으면 (값, true), 그대로 두면 (_, false).
func (p *parser) substituteString(s, path string, secret bool) (string, bool) {
	if s == "" {
		return "", false
	}
	m := refPattern.FindStringSubmatch(s)
	if m == nil {
		if secret {
			// 값을 메시지에 넣지 않는다.
			p.errs.add("R4", path, "secret 은 ${env:NAME} 또는 ${file:PATH} 참조여야 함")
		}
		return "", false
	}
	return p.resolveRef(path, m[1], m[2])
}

// resolveRef 는 참조 하나를 값으로 바꾼다. 대상이 없거나 비어 있으면 R5. [R5]
// `${file:}` 은 앞뒤 공백을 제거한다: `echo token > file` 로 만든 파일의 끝 개행이
// 토큰에 섞이는 것을 막기 위해서다. PEM 은 내부 개행이 보존되고 끝 개행은 의미가 없다.
func (p *parser) resolveRef(path, kind, arg string) (string, bool) {
	switch kind {
	case "env":
		val, ok := p.env.LookupEnv(arg)
		if !ok || val == "" {
			p.errs.addf("R5", path, "환경변수 %s 가 없거나 비어 있음", arg)
			return "", false
		}
		return val, true
	case "file":
		file, err := p.expandHome(arg)
		if err != nil {
			p.errs.addf("R5", path, "파일 경로 %s: %v", arg, err)
			return "", false
		}
		b, err := p.env.ReadFile(file)
		if err != nil {
			p.errs.addf("R5", path, "파일 %s 읽기 실패: %v", file, err)
			return "", false
		}
		val := strings.TrimSpace(string(b))
		if val == "" {
			p.errs.addf("R5", path, "파일 %s 이 비어 있음", file)
			return "", false
		}
		return val, true
	}
	return "", false
}

// expandHome 은 "~" 와 "~/..." 를 홈 디렉터리로 바꾼다. 그 외는 그대로.
func (p *parser) expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := p.env.HomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

func yamlName(f reflect.StructField) (name string, inline bool) {
	tag := f.Tag.Get("yaml")
	parts := strings.Split(tag, ",")
	name = parts[0]
	for _, opt := range parts[1:] {
		if opt == "inline" {
			inline = true
		}
	}
	if name == "" {
		name = strings.ToLower(f.Name)
	}
	return name, inline
}

func joinPath(base, name string) string {
	if base == "" {
		return name
	}
	return base + "." + name
}
