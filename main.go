package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

var (
	addr = flag.String("http", ":8080", "listen on http")
)

type Index struct {
	Dir   string
	Data  []byte
	Files map[string]*File
}

type File struct {
	Path    string
	AbsPath string
	Lines   []Line
}

type Line struct {
	Number int
	From   int
	To     int
}

func NewIndex(dir string, data []byte) *Index {
	index := &Index{}
	index.Dir = dir
	index.Data = data
	index.Files = make(map[string]*File)
	return index
}

func NewFile(dir string, path string) *File {
	file := &File{}
	file.Path = path
	if filepath.IsAbs(path) {
		file.AbsPath = path
	} else {
		file.AbsPath = filepath.Join(dir, path)
	}
	return file
}

func indexByteAt(data []byte, at int, b byte) int {
	s := bytes.IndexByte(data[at:], b)
	if s < 0 {
		return s
	}
	return s + at
}

func (index *Index) Parse() {
	lineStart := 0
	lineEnd := 0
	for lineStart < len(index.Data) {
		lineEnd = indexByteAt(index.Data, lineStart, '\n')
		if lineEnd < 0 {
			lineEnd = len(index.Data)
		}

		index.Add(lineStart, lineEnd)
		lineStart = lineEnd + 1
	}

	for _, file := range index.Files {
		sort.Slice(file.Lines, func(i, k int) bool {
			if file.Lines[i].Number == file.Lines[k].Number {
				if file.Lines[i].From == file.Lines[k].From {
					return file.Lines[i].To < file.Lines[k].To
				}
				return file.Lines[i].From < file.Lines[k].From
			}
			return file.Lines[i].Number < file.Lines[k].Number
		})
	}
}

func (index *Index) Add(lineStart, lineEnd int) {
	line := index.Data[lineStart:lineEnd]
	if len(line) <= 2 {
		return
	}

	if line[0] == '\t' {
		return
	}
	if bytes.HasPrefix(line, []byte(".   ")) {
		return
	}
	if bytes.HasPrefix(line, []byte(`<autogenerated>`)) {
		return
	}
	if bytes.HasPrefix(line, []byte(`typecheck`)) {
		return
	}
	if bytes.HasPrefix(line, []byte(`escwalk:`)) {
		return
	}
	if bytes.HasPrefix(line, []byte(`escflood:`)) {
		return
	}
	if bytes.HasPrefix(line, []byte(`substituting name`)) {
		return
	}
	if bytes.HasPrefix(line, []byte(`not substituting name`)) {
		return
	}

	// C:\Go\src\example\abc.go:688: cannot inline ...
	// C:\Go\src\example\abc.go:688:123: cannot inline ...
	firstSeparator := indexByteAt(line, 2, ':') // starting
	if firstSeparator < 0 {
		return
	}
	secondSeparator := indexByteAt(line, firstSeparator+1, ':')
	if secondSeparator < 0 {
		return
	}
	thirdSeparator := indexByteAt(line, secondSeparator+1, ' ')
	if thirdSeparator < 0 {
		return
	}

	path := string(line[:firstSeparator])
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}

	file, ok := index.Files[path]
	if !ok {
		file = NewFile(index.Dir, path)
		index.Files[path] = file
	}

	lineNumber, err := strconv.Atoi(string(line[firstSeparator+1 : secondSeparator]))
	if err != nil {
		return
	}
	file.Lines = append(file.Lines, Line{
		Number: lineNumber,
		From:   lineStart + thirdSeparator,
		To:     lineEnd,
	})
}

type FileInfo struct {
	Path    string     `json:"path"`
	AbsPath string     `json:"path"`
	Lines   []LineInfo `json:"lines"`
}

type LineInfo struct {
	Number  int      `json:"number"`
	Content string   `json:"content"`
	Info    []string `json:"info"`
}

func (index *Index) FileInfo(path string) (*FileInfo, error) {
	file, ok := index.Files[path]
	if !ok {
		return nil, errors.New("not found")
	}

	data, err := ioutil.ReadFile(file.AbsPath)
	if err != nil {
		return nil, err
	}

	fileinfo := &FileInfo{}
	fileinfo.Path = file.Path
	fileinfo.AbsPath = file.AbsPath

	infoIndex := 0
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		lineinfo := LineInfo{}
		lineinfo.Number = i + 1
		lineinfo.Content = line
		lineinfo.Info = []string{}

		lineNumber := i + 1
		for infoIndex < len(file.Lines) && lineNumber > file.Lines[infoIndex].Number {
			infoIndex++
		}
		for infoIndex < len(file.Lines) && lineNumber == file.Lines[infoIndex].Number {
			x := file.Lines[infoIndex]
			lineinfo.Info = append(lineinfo.Info, string(index.Data[x.From:x.To]))
			infoIndex++
		}

		fileinfo.Lines = append(fileinfo.Lines, lineinfo)
	}

	return fileinfo, nil
}

func main() {
	flag.Parse()
	var rd io.Reader = os.Stdin
	if flag.Arg(0) != "" {
		file, err := os.Open(flag.Arg(0))
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		defer file.Close()
		rd = file
	}

	data, err := ioutil.ReadAll(rd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	dir, _ := filepath.Abs(".")

	index := NewIndex(dir, data)
	index.Parse()

	fmt.Printf("Listening on %v\n", *addr)
	err = http.ListenAndServe(*addr, &Server{index})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

type Server struct {
	Index *Index
}

func (server *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "" || r.URL.Path == "/" {
		err := T.Execute(w, server.Index.Files)
		if err != nil {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(os.Stderr, "%v\n", err)
		}
		return
	}

	if r.URL.Path == "/file" {
		path := r.FormValue("path")
		if path == "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "No path specified.")
			return
		}

		fileinfo, err := server.Index.FileInfo(path)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(os.Stderr, "%v\n", err)
			fmt.Fprintf(w, "Error: %v", err)
			return
		}

		w.Header().Add("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		err = json.NewEncoder(w).Encode(fileinfo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
		}
		return
	}

	w.WriteHeader(http.StatusNotFound)
}

var T = template.Must(template.New("").Parse(`
<html>
<body>
	<select id="file" onchange="fileSelected()">
		{{ range . }}
		<option value="{{.Path}}">{{.AbsPath}}</option>
		{{ end }}
	</select>
	<div id="source">
	</div>

	<style>
	.line {
		position: relative;
		height: 1.2em;
		overflow: hidden;

		--number-width: 3em;
		--info-width: 20em;
		--tags-width: 3em;

		contain: strict;
	}
	.line:hover {
		background: #eee;
	}
	
	.line .number {
		position: absolute;
		display: block;
		left: 0; right: 0; top: 0; bottom: 0;
		width: var(--number-width);
	}
	.line .content {
		position: absolute;
		display: block;
		white-space: pre;
		left: var(--number-width);
		right: calc(var(--info-width) + var(--tags-width));
		top: 0; bottom: 0;
		text-overflow: ellipsis;
		overflow: hidden;
	}
	.line .info {
		position: absolute;
		display: block;
		right: var(--tags-width); top: 0; bottom: 0;
		width: var(--info-width);
		text-overflow: ellipsis;
		overflow: hidden;
	}

	.line .tag {
		position: absolute;
		display: block;
		top: 0; bottom: 0;
		width: 1em;
		overflow: hidden;
		border: 1px solid #eee;

		text-align: center;
	}
	.line .tag1 { right: 0em; }
	.line .tag1.active { background: #cef9ce; }
	.line .tag2 { right: 1em; }
	.line .tag2.active { background: #ffbdbd; }
	.line .tag3 { right: 2em; }
	.line .tag3.active { background: #bdbdff; }
	</style>

	<script>
		var pending = null;
		function fileSelected() {
			if(pending){
				pending.abort();
			}
			var el = document.getElementById("file")
			if(el.value != ""){
				pending = fetch("/file?path=" + encodeURI(el.value))
					.then(function(response){
						pending = null;
						if(response.ok){
							response.json().then(updateSource);
						}
					})
			}
		}

		function updateSource(file) {
			var fragment = document.createDocumentFragment();
			file.lines.forEach(line => {
				var lineel = h("div", "line");
				lineel.appendChild(h("span", "number", line.number));
				lineel.appendChild(h("span", "content", line.content));
	
				var fullinfo = "";
				if(line.info.length > 0){
					var infoel = h("span", "info", line.info[0]);
					fullinfo = line.info.join("\n");
					infoel.title = fullinfo;
					lineel.appendChild(infoel);
				}

				function addtag(i, f, match){
					if(fullinfo.match(match)){
						var el = h("span", "tag active tag" + i, f);
						el.title = match;
						lineel.appendChild(el);
					} else {
						lineel.appendChild(h("span", "tag tag" + i), "");
					}
				}

				addtag(1, "I", "inlining");
				addtag(2, "X", "cannot inline");
				addtag(3, "H", "escapes to heap");

				fragment.appendChild(lineel);
			});

			var source = document.getElementById("source");
			source.innerText = "";
			source.appendChild(fragment);
		}

		function h(tag, className, text){
			var el = document.createElement(tag);
			el.className = className;
			if(text){
				el.innerText = text;
			}
			return el;
		}

		fileSelected();
	</script>
</body>
</html>
`))
