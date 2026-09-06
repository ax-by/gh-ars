package config

import (
	"encoding/binary"
	"encoding/pem"
	"errors"
	"strings"
)

// keyEncrypted 는 SSH 개인키 파일이 passphrase 로 암호화되어 있는지 본다. [R19]
// golang.org/x/crypto/ssh 는 Phase 8 에서 도입하므로(DESIGN §10) 여기서는 PEM 머리만 읽는다.
// 판정은 ssh.ParseRawPrivateKey 가 PassphraseMissingError 를 내는 조건과 같다:
//   - OPENSSH PRIVATE KEY: openssh-key-v1 블롭의 ciphername/kdfname 이 "none" 이 아니면 암호화
//   - ENCRYPTED PRIVATE KEY (PKCS#8): 암호화
//   - Proc-Type: 4,ENCRYPTED 헤더 (전통 PEM): 암호화
//   - 그 외 PEM 블록: 평문
func keyEncrypted(data []byte) (bool, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return false, errors.New("PEM 형식의 개인키가 아님")
	}
	switch block.Type {
	case "OPENSSH PRIVATE KEY":
		return opensshEncrypted(block.Bytes)
	case "ENCRYPTED PRIVATE KEY":
		return true, nil
	}
	if strings.Contains(block.Headers["Proc-Type"], "ENCRYPTED") {
		return true, nil
	}
	return false, nil
}

const opensshMagic = "openssh-key-v1\x00"

// opensshEncrypted 는 openssh-key-v1 블롭의 첫 두 문자열(ciphername, kdfname)을 읽는다.
func opensshEncrypted(b []byte) (bool, error) {
	if !strings.HasPrefix(string(b), opensshMagic) {
		return false, errors.New("openssh-key-v1 magic 이 아님")
	}
	rest := b[len(opensshMagic):]
	var names [2]string
	for i := range names {
		if len(rest) < 4 {
			return false, errors.New("openssh-key-v1 머리가 짧음")
		}
		n := int(binary.BigEndian.Uint32(rest))
		rest = rest[4:]
		if n < 0 || len(rest) < n {
			return false, errors.New("openssh-key-v1 머리가 짧음")
		}
		names[i] = string(rest[:n])
		rest = rest[n:]
	}
	return names[0] != "none" || names[1] != "none", nil
}
