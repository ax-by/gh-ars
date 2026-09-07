package runtime

import (
	"archive/tar"
	"bytes"
	"errors"
)

// JIT config 전달 경로. 래퍼(wrapper.go)가 읽는 경로와 일치해야 한다. [§7.2-4]
const (
	// RunnerHome 은 `cp -` 의 대상 디렉터리다. 공식 이미지의 runner 사용자 홈. [§7.2-4, §9.1]
	RunnerHome  = "/home/runner"
	jitFileName = ".jitconfig"
	jitFileMode = 0o600
	// 공식 runner 이미지의 runner 사용자 uid/gid. 래퍼가 rm -f 할 수 있어야 한다. [§7.2-4]
	runnerUID = 1001
	runnerGID = 1001
)

var errEmptyJIT = errors.New("runtime: 빈 JIT config")

// JITTar 는 `.jitconfig` 1개짜리 tar 스트림을 메모리에서 만든다. [§7.2-4, DESIGN §4.2]
// 내용은 encoded + "\n"(래퍼의 `read -r` 이 줄 끝을 기대한다). 반환 Reader 는 메모리
// 버퍼라 즉시 반환한다 — Executor 가 stdin 을 *os.File 로 넘기며 요구하는 조건이다.
// [DESIGN §4.1]
func JITTar(encoded string) (*bytes.Reader, error) {
	if encoded == "" {
		return nil, errEmptyJIT
	}
	body := []byte(encoded + "\n")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     jitFileName,
		Mode:     jitFileMode,
		Uid:      runnerUID,
		Gid:      runnerGID,
		Size:     int64(len(body)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, err
	}
	if _, err := tw.Write(body); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return bytes.NewReader(buf.Bytes()), nil
}
