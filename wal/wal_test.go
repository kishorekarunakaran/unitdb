package wal

import (
	"os"
	"testing"
)

func newTestWal(path string, del bool) (*WAL, bool, error) {
	logOpts := Options{Path: path + ".log", TargetSize: 1 << 30}
	if del {
		os.Remove(logOpts.Path)
	}
	return New(logOpts)
}

func TestEmptyLog(t *testing.T) {
	wal, needRecover, err := newTestWal("test.db", true)
	if needRecover || err != nil {
		t.Fatal(err)
	}
	defer wal.Close()
	seqs, err := wal.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(seqs) != 0 {
		t.Fatalf("Write ahead log non-empty, seqs %d", seqs)
	}
}

func TestRecovery(t *testing.T) {
	wal, needRecovery, err := newTestWal("test.db", true)
	if err != nil {
		t.Fatal(err)
	}
	defer wal.Close()

	if needRecovery {
		t.Fatalf("Write ahead log non-empty")
	}

	var i byte
	var n uint8 = 255

	logWriter, err := wal.NewWriter()
	if err != nil {
		t.Fatal(err)
	}

	for i = 0; i < n; i++ {
		val := []byte("msg.")
		val = append(val, i)
		if err := <-logWriter.Append(val); err != nil {
			t.Fatal(err)
		}
	}

	logSeq := wal.NextSeq()
	if err := <-logWriter.SignalInitWrite(logSeq); err != nil {
		t.Fatal(err)
	}

	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}

	wal, needRecovery, err = newTestWal("test.db", false)
	if !needRecovery || err != nil {
		t.Fatal(err)
	}
}

func TestLogApplied(t *testing.T) {
	wal, needRecovery, err := newTestWal("test.db", true)
	if err != nil {
		t.Fatal(err)
	}
	defer wal.Close()
	var i byte
	var n uint8 = 255

	logWriter, err := wal.NewWriter()
	if err != nil {
		t.Fatal(err)
	}

	for i = 0; i < n; i++ {
		val := []byte("msg.")
		val = append(val, i)
		if err := <-logWriter.Append(val); err != nil {
			t.Fatal(err)
		}
	}

	logSeq := wal.NextSeq()
	if err := <-logWriter.SignalInitWrite(logSeq); err != nil {
		t.Fatal(err)
	}

	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}

	wal, needRecovery, err = newTestWal("test.db", false)
	if !needRecovery || err != nil {
		t.Fatal(err)
	}

	seqs, err := wal.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(seqs) == 0 {
		t.Fatalf("Write ahead log is empty, seqs %d", seqs)
	}

	for _, s := range seqs {
		it, err := wal.Read(s)
		if err != nil {
			t.Fatal(err)
		}

		_, ok := it.Next()
		if !ok {
			break
		}

		if err := wal.SignalLogApplied(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}

	wal, needRecovery, err = newTestWal("test.db", false)
	if needRecovery || err != nil {
		t.Fatal(err)
	}
}

func TestSimple(t *testing.T) {
	wal, _, err := newTestWal("test.db", true)
	if err != nil {
		t.Fatal(err)
	}
	defer wal.Close()

	var i byte
	var n uint8 = 255

	logWriter, err := wal.NewWriter()
	if err != nil {
		t.Fatal(err)
	}

	for i = 0; i < n; i++ {
		val := []byte("msg.")
		val = append(val, i)
		if err := <-logWriter.Append(val); err != nil {
			t.Fatal(err)
		}
	}

	logSeq := wal.NextSeq()
	if err := <-logWriter.SignalInitWrite(logSeq); err != nil {
		t.Fatal(err)
	}
}