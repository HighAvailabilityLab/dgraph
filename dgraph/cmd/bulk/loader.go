/*
 * Copyright 2017-2018 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package bulk

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/dgraph-io/badger"
	bo "github.com/dgraph-io/badger/options"
	"github.com/dgraph-io/dgraph/protos/pb"
	"github.com/dgraph-io/dgraph/schema"
	"github.com/dgraph-io/dgraph/x"
	"github.com/dgraph-io/dgraph/xidmap"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
)

type options struct {
	RDFDir        string
	JSONDir       string
	SchemaFile    string
	DgraphsDir    string
	TmpDir        string
	NumGoroutines int
	MapBufSize    int64
	ExpandEdges   bool
	SkipMapPhase  bool
	CleanupTmp    bool
	NumShufflers  int
	Version       bool
	StoreXids     bool
	ZeroAddr      string
	HttpAddr      string
	IgnoreErrors  bool

	MapShards    int
	ReduceShards int

	shardOutputDirs []string
}

const (
	rdfInput int = iota
	jsonInput
)

type state struct {
	opt           options
	prog          *progress
	xids          *xidmap.XidMap
	schema        *schemaStore
	shards        *shardMap
	readerChunkCh chan *bytes.Buffer
	mapFileId     uint32 // Used atomically to name the output files of the mappers.
	dbs           []*badger.DB
	writeTs       uint64 // All badger writes use this timestamp
}

type loader struct {
	*state
	mappers []*mapper
	xidDB   *badger.DB
	zero    *grpc.ClientConn
}

func newLoader(opt options) *loader {
	fmt.Printf("Connecting to zero at %s\n", opt.ZeroAddr)
	zero, err := grpc.Dial(opt.ZeroAddr,
		grpc.WithBlock(),
		grpc.WithInsecure(),
		grpc.WithTimeout(time.Minute))
	x.Checkf(err, "Unable to connect to zero, Is it running at %s?", opt.ZeroAddr)
	st := &state{
		opt:    opt,
		prog:   newProgress(),
		shards: newShardMap(opt.MapShards),
		// Lots of gz readers, so not much channel buffer needed.
		readerChunkCh: make(chan *bytes.Buffer, opt.NumGoroutines),
		writeTs:       getWriteTimestamp(zero),
	}
	st.schema = newSchemaStore(readSchema(opt.SchemaFile), opt, st)
	ld := &loader{
		state:   st,
		mappers: make([]*mapper, opt.NumGoroutines),
		zero:    zero,
	}
	for i := 0; i < opt.NumGoroutines; i++ {
		ld.mappers[i] = newMapper(st)
	}
	go ld.prog.report()
	return ld
}

func getWriteTimestamp(zero *grpc.ClientConn) uint64 {
	client := pb.NewZeroClient(zero)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		ts, err := client.Timestamps(ctx, &pb.Num{Val: 1})
		cancel()
		if err == nil {
			return ts.GetStartId()
		}
		fmt.Printf("Error communicating with dgraph zero, retrying: %v", err)
		time.Sleep(time.Second)
	}
}

func readSchema(filename string) []*pb.SchemaUpdate {
	f, err := os.Open(filename)
	x.Check(err)
	defer f.Close()
	var r io.Reader = f
	if filepath.Ext(filename) == ".gz" {
		r, err = gzip.NewReader(f)
		x.Check(err)
	}

	buf, err := ioutil.ReadAll(r)
	x.Check(err)

	initialSchema, err := schema.Parse(string(buf))
	x.Check(err)
	return initialSchema
}

type chunker interface {
	begin(r *bufio.Reader) error
	chunk(r *bufio.Reader) (*bytes.Buffer, error)
	end(r *bufio.Reader) error
}

type rdfChunker struct{}
type jsonChunker struct{}

func newChunker(inputFormat int) chunker {
	switch inputFormat {
	case rdfInput:
		return &rdfChunker{}
	case jsonInput:
		return &jsonChunker{}
	default:
		panic("unknown loader type")
	}
}

func (_ rdfChunker) begin(r *bufio.Reader) error {
	return nil
}

func (_ rdfChunker) chunk(r *bufio.Reader) (*bytes.Buffer, error) {
	batch := new(bytes.Buffer)
	batch.Grow(1 << 20)
	for lineCount := 0; lineCount < 1e5; lineCount++ {
		slc, err := r.ReadSlice('\n')
		if err == io.EOF {
			batch.Write(slc)
			return batch, err
		}
		if err == bufio.ErrBufferFull {
			// This should only happen infrequently.
			batch.Write(slc)
			var str string
			str, err = r.ReadString('\n')
			if err == io.EOF {
				batch.WriteString(str)
				return batch, err
			}
			if err != nil {
				return nil, err
			}
			batch.WriteString(str)
			continue
		}
		if err != nil {
			return nil, err
		}
		batch.Write(slc)
	}
	return batch, nil
}

func (_ rdfChunker) end(r *bufio.Reader) error {
	return nil
}

func slurpSpace(r *bufio.Reader) error {
	for {
		ch, _, err := r.ReadRune()
		if err != nil {
			return err
		}
		if !unicode.IsSpace(ch) {
			r.UnreadRune()
			break
		}
	}
	return nil
}

func slurpQuoted(r *bufio.Reader, out *bytes.Buffer) error {
	for {
		ch, _, err := r.ReadRune()
		if err != nil {
			return err
		}
		x.Check2(out.WriteRune(ch))

		if ch == '\\' {
			// Pick one more rune.
			if esc, _, err := r.ReadRune(); err != nil {
				return err
			} else {
				x.Check2(out.WriteRune(esc))
				continue
			}
		}
		if ch == '"' {
			return nil
		}
	}
}

func (_ jsonChunker) begin(r *bufio.Reader) error {
	// The JSON file to load must be an array of maps (that is, '[ { ... }, { ... }, ... ]').
	// This function must be called before calling readJSONChunk for the first time to advance
	// the Reader past the array start token ('[') so that calls to readJSONChunk can read
	// one array element at a time instead of having to read the entire array into memory.
	if err := slurpSpace(r); err != nil {
		return err
	}

	ch, _, err := r.ReadRune()
	if err != nil {
		return err
	}
	if ch != '[' {
		return fmt.Errorf("json file must contain array. Found: %v", ch)
	}
	return nil
}

func (_ jsonChunker) chunk(r *bufio.Reader) (*bytes.Buffer, error) {
	out := new(bytes.Buffer)
	out.Grow(1 << 20)

	// For RDF, the loader just reads the input and the mapper parses it into nquads,
	// so do the same for JSON. But since JSON is not line-oriented like RDF, it's a little
	// more complicated to ensure a complete JSON structure is read.

	if err := slurpSpace(r); err != nil {
		return out, err
	}
	ch, _, err := r.ReadRune()
	if err != nil {
		return out, err
	}
	if ch != '{' {
		return nil, fmt.Errorf("expected json map start. Found: %v", ch)
	}
	x.Check2(out.WriteRune(ch))

	// Just find the matching closing brace. Let the JSON-to-nquad parser in the mapper worry
	// about whether everything in between is valid JSON or not.

	depth := 1 // We already consumed one `{`, so our depth starts at one.
	for depth > 0 {
		ch, _, err = r.ReadRune()
		if err != nil {
			return nil, errors.New("malformed json")
		}
		x.Check2(out.WriteRune(ch))

		switch ch {
		case '{':
			depth++
		case '}':
			depth--
		case '"':
			if err := slurpQuoted(r, out); err != nil {
				return nil, err
			}
		default:
			// We just write the rune to out, and let the Go JSON parser do its job.
		}
	}

	// The map should be followed by either the ',' between array elements, or the ']'
	// at the end of the array.
	if err := slurpSpace(r); err != nil {
		return nil, err
	}
	ch, _, err = r.ReadRune()
	if err != nil {
		return nil, err
	}
	switch ch {
	case ']':
		return out, io.EOF
	case ',':
		// pass
	default:
		// Let next call to this function report the error.
		x.Check(r.UnreadRune())
	}
	return out, nil
}

func (_ jsonChunker) end(r *bufio.Reader) error {
	if slurpSpace(r) == io.EOF {
		return nil
	} else {
		return errors.New("not all of json file consumed")
	}
}

func findDataFiles(dir string, ext string) []string {
	var files []string
	x.Check(filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, ext) || strings.HasSuffix(path, ext+".gz") {
			files = append(files, path)
		}
		return nil
	}))
	return files
}

type uidRangeResponse struct {
	uids *pb.AssignedIds
	err  error
}

func (ld *loader) mapStage() {
	ld.prog.setPhase(mapPhase)

	xidDir := filepath.Join(ld.opt.TmpDir, "xids")
	x.Check(os.Mkdir(xidDir, 0755))
	opt := badger.DefaultOptions
	opt.SyncWrites = false
	opt.TableLoadingMode = bo.MemoryMap
	opt.Dir = xidDir
	opt.ValueDir = xidDir
	var err error
	ld.xidDB, err = badger.Open(opt)
	x.Check(err)
	ld.xids = xidmap.New(ld.xidDB, ld.zero, xidmap.Options{
		NumShards: 1 << 10,
		LRUSize:   1 << 19,
	})

	var files []string
	var ext string
	var loaderType int
	if ld.opt.RDFDir != "" {
		loaderType = rdfInput
		ext = ".rdf"
		files = findDataFiles(ld.opt.RDFDir, ext)
	} else {
		loaderType = jsonInput
		ext = ".json"
		files = findDataFiles(ld.opt.JSONDir, ext)
	}

	readers := make(map[string]*bufio.Reader)
	for _, file := range files {
		f, err := os.Open(file)
		x.Check(err)
		defer f.Close()
		// TODO detect compressed input instead of relying on filename
		//      so data can be streamed in
		if !strings.HasSuffix(file, ".gz") {
			readers[file] = bufio.NewReaderSize(f, 1<<20)
		} else {
			gzr, err := gzip.NewReader(f)
			x.Checkf(err, "Could not create gzip reader for file %q.", file)
			readers[file] = bufio.NewReader(gzr)
		}
	}

	if len(readers) == 0 {
		fmt.Printf("No *%s files found.\n", ext)
		os.Exit(1)
	}

	var mapperWg sync.WaitGroup
	mapperWg.Add(len(ld.mappers))
	for _, m := range ld.mappers {
		go func(m *mapper) {
			m.run(loaderType)
			mapperWg.Done()
		}(m)
	}

	// This is the main map loop.
	thr := x.NewThrottle(ld.opt.NumGoroutines)
	var fileCount int
	for file, r := range readers {
		thr.Start()
		fileCount++
		fmt.Printf("Processing file (%d out of %d): %s\n", fileCount, len(readers), file)
		chunker := newChunker(loaderType)
		go func(r *bufio.Reader) {
			defer thr.Done()
			x.Check(chunker.begin(r))
			for {
				chunkBuf, err := chunker.chunk(r)
				if chunkBuf != nil && chunkBuf.Len() > 0 {
					ld.readerChunkCh <- chunkBuf
				}
				if err == io.EOF {
					break
				} else if err != nil {
					x.Check(err)
				}
			}
			x.Check(chunker.end(r))
		}(r)
	}
	thr.Wait()

	close(ld.readerChunkCh)
	mapperWg.Wait()

	// Allow memory to GC before the reduce phase.
	for i := range ld.mappers {
		ld.mappers[i] = nil
	}
	ld.xids.EvictAll()
	x.Check(ld.xidDB.Close())
	ld.xids = nil
	runtime.GC()
}

type shuffleOutput struct {
	db         *badger.DB
	mapEntries []*pb.MapEntry
}

func (ld *loader) reduceStage() {
	ld.prog.setPhase(reducePhase)

	shuffleOutputCh := make(chan shuffleOutput, 100)
	go func() {
		shuf := shuffler{state: ld.state, output: shuffleOutputCh}
		shuf.run()
	}()

	redu := reducer{
		state:     ld.state,
		input:     shuffleOutputCh,
		writesThr: x.NewThrottle(100),
	}
	redu.run()
}

func (ld *loader) writeSchema() {
	for _, db := range ld.dbs {
		ld.schema.write(db)
	}
}

func (ld *loader) cleanup() {
	for _, db := range ld.dbs {
		x.Check(db.Close())
	}
	ld.prog.endSummary()
}
