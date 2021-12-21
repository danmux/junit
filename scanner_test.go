package testresults_test

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/assert/cmp"

	. "github.com/danmux/junit"
)

func TestScanner_TestData(t *testing.T) {
	err := filepath.Walk("testdata", func(path string, info fs.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}
		s := Scanner{}
		file, err := os.Open(path)
		assert.NilError(t, err)
		s.Start(context.Background(), file)
		cases := 0
		for {
			test, err := s.Scan()
			assert.NilError(t, err)
			if test == nil {
				break
			}
			cases++
		}
		t.Log(info.Name(), cases)
		return nil
	})
	assert.NilError(t, err)
}

func TestScanner_LargeFiles(t *testing.T) {
	// This is intended as a mainly manual test to get a feel for the
	// performance under excessive conditions - mainly to check that the
	// RSS is bounded independently of the size of the files being processed.

	// start up a chunked reader pipe which we can push a file through
	// multiple times into one scanner emulating a huge file without
	// having to commit one
	r, wr := io.Pipe()
	const chunk = 32768
	b := make([]byte, chunk)
	total := 0
	fw := func(f io.Reader) {
		for {
			n, err := f.Read(b)
			if n > 0 {
				_, _ = wr.Write(b[:n])
			}
			total += n
			if err == io.EOF {
				return
			}
		}
	}
	// start repeatedly writing the file to the pipe
	go func() {
		for i := 0; i < 40; i++ {
			xmlFile, err := os.Open("testdata/junit/sample3.xml")
			assert.NilError(t, err)

			fw(xmlFile)
			_ = xmlFile.Close()
		}
		_ = wr.Close()
	}()

	// scan the reader side of the pipe
	start := time.Now()
	s := Scanner{}
	s.Start(context.Background(), r)
	cases := 0
	for {
		test, err := s.Scan()
		assert.NilError(t, err)
		if test == nil {
			break
		}
		cases++
	}
	assert.Check(t, cmp.Equal(27440, cases))

	// log out how long it took and how much we processed
	t.Log(time.Since(start))
	t.Log(total)
	// Some prior run results...
	// 64,422,675  60Mb in 2.75s rss 9M
	// 515,381,400 490Mb in 17.202169459s rss 9M
}
