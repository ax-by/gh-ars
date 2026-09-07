package runtime

import (
	"archive/tar"
	"errors"
	"io"
	"testing"
)

// [§7.2-4] tar 스트림은 `.jitconfig` 엔트리 하나, mode 0600, uid/gid 1001, 내용 = encoded + "\n".
// 경로가 틀리면 래퍼가 못 읽고, 소유자가 틀리면 래퍼의 rm -f 가 실패해 파일이 남는다.
func TestJITTar_S7_2_4_SingleEntry(t *testing.T) {
	const jit = "eyJmYWtlIjoidmFsdWUifQ=="
	r, err := JITTar(jit)
	if err != nil {
		t.Fatalf("JITTar: %v", err)
	}
	tr := tar.NewReader(r)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("첫 엔트리 없음: %v", err)
	}
	if hdr.Name != ".jitconfig" {
		t.Errorf("Name = %q, want .jitconfig", hdr.Name)
	}
	if hdr.Typeflag != tar.TypeReg {
		t.Errorf("Typeflag = %v, want 일반 파일", hdr.Typeflag)
	}
	if hdr.Mode != 0o600 {
		t.Errorf("Mode = %o, want 0600", hdr.Mode)
	}
	if hdr.Uid != 1001 || hdr.Gid != 1001 {
		t.Errorf("Uid/Gid = %d/%d, want 1001/1001", hdr.Uid, hdr.Gid)
	}
	body, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("본문 읽기: %v", err)
	}
	if string(body) != jit+"\n" {
		t.Errorf("본문 = %q, want %q", body, jit+"\n")
	}
	if _, err := tr.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("엔트리가 하나가 아니다: %v", err)
	}
}

// [§7.2-4] cp - 의 대상 경로는 /home/runner 다. 래퍼가 /home/runner/.jitconfig 를 읽는다.
func TestJITTar_S7_2_4_DestDir(t *testing.T) {
	if RunnerHome != "/home/runner" {
		t.Fatalf("RunnerHome = %q, want /home/runner", RunnerHome)
	}
}

// [§7.2-4] 빈 JIT 는 GenerateJIT 실패의 흔적이다. 빈 파일을 컨테이너에 넣지 않는다.
func TestJITTar_S7_2_4_EmptyRejected(t *testing.T) {
	if _, err := JITTar(""); !errors.Is(err, errEmptyJIT) {
		t.Fatalf("err = %v, want errEmptyJIT", err)
	}
}
