package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parseString(t *testing.T, body string) ([]Instruction, error) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "Docksmithfile")
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return ParseFile(p)
}

func TestParseAllInstructions(t *testing.T) {
	instrs, err := parseString(t, `FROM busybox:latest
ENV APP_ENV=production
WORKDIR /app
COPY src/main.sh /app/main.sh
RUN echo hello > /app/out.txt
CMD ["/bin/sh", "/app/main.sh"]
`)
	if err != nil {
		t.Fatal(err)
	}
	want := []InstructionType{InstrFROM, InstrENV, InstrWORKDIR, InstrCOPY, InstrRUN, InstrCMD}
	if len(instrs) != len(want) {
		t.Fatalf("parsed %d instructions, want %d", len(instrs), len(want))
	}
	for i, w := range want {
		if instrs[i].Type != w {
			t.Errorf("instruction %d = %s, want %s", i, instrs[i].Type, w)
		}
	}
	if instrs[0].LineNum != 1 || instrs[5].LineNum != 6 {
		t.Errorf("line numbers wrong: %d, %d", instrs[0].LineNum, instrs[5].LineNum)
	}
}

func TestParseSkipsBlankLinesAndComments(t *testing.T) {
	instrs, err := parseString(t, `# leading comment

FROM busybox:latest

   # indented comment
RUN echo hi
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(instrs) != 2 {
		t.Fatalf("parsed %d instructions, want 2", len(instrs))
	}
	// Line numbers must still refer to the real file, for error messages.
	if instrs[0].LineNum != 3 {
		t.Errorf("FROM on line %d, want 3", instrs[0].LineNum)
	}
	if instrs[1].LineNum != 6 {
		t.Errorf("RUN on line %d, want 6", instrs[1].LineNum)
	}
}

func TestParseKeywordsAreCaseInsensitive(t *testing.T) {
	instrs, err := parseString(t, "from busybox:latest\nRun echo hi\n")
	if err != nil {
		t.Fatal(err)
	}
	if instrs[0].Type != InstrFROM || instrs[1].Type != InstrRUN {
		t.Errorf("case-insensitive keywords not normalised: %v", instrs)
	}
}

func TestParseRejectsUnknownInstruction(t *testing.T) {
	_, err := parseString(t, "FROM busybox:latest\nMAGIC do-something\n")
	if err == nil {
		t.Fatal("expected an error for an unknown instruction")
	}
	if !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "MAGIC") {
		t.Errorf("error should name the line and the keyword, got: %v", err)
	}
}

func TestParseMissingFile(t *testing.T) {
	if _, err := ParseFile(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("expected an error for a missing Docksmithfile")
	}
}

func TestAsFROMDefaultsTagToLatest(t *testing.T) {
	i := Instruction{Type: InstrFROM, Args: "busybox"}
	got, err := i.AsFROM()
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "busybox" || got.Tag != "latest" {
		t.Errorf("AsFROM = %+v, want {busybox latest}", got)
	}

	i = Instruction{Type: InstrFROM, Args: "alpine:3.19"}
	got, err = i.AsFROM()
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "alpine" || got.Tag != "3.19" {
		t.Errorf("AsFROM = %+v, want {alpine 3.19}", got)
	}
}

func TestAsCOPY(t *testing.T) {
	i := Instruction{Type: InstrCOPY, Args: "src/main.sh /app/main.sh", LineNum: 4}
	got, err := i.AsCOPY()
	if err != nil {
		t.Fatal(err)
	}
	if got.Src != "src/main.sh" || got.Dest != "/app/main.sh" {
		t.Errorf("AsCOPY = %+v", got)
	}

	if _, err := (&Instruction{Type: InstrCOPY, Args: "only-one", LineNum: 7}).AsCOPY(); err == nil {
		t.Error("COPY with a single argument should be an error")
	} else if !strings.Contains(err.Error(), "line 7") {
		t.Errorf("error should name the line, got: %v", err)
	}
}

func TestAsENV(t *testing.T) {
	// Values may legitimately contain '=' — only the first one separates.
	i := Instruction{Type: InstrENV, Args: "CONN=postgres://h/db?a=b"}
	got, err := i.AsENV()
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != "CONN" || got.Value != "postgres://h/db?a=b" {
		t.Errorf("AsENV = %+v", got)
	}

	if _, err := (&Instruction{Type: InstrENV, Args: "NOEQUALS", LineNum: 3}).AsENV(); err == nil {
		t.Error("ENV without '=' should be an error")
	}
}

func TestAsCMDRequiresJSONArray(t *testing.T) {
	i := Instruction{Type: InstrCMD, Args: `["/bin/sh", "-c", "echo hi"]`}
	got, err := i.AsCMD()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "/bin/sh" || got[2] != "echo hi" {
		t.Errorf("AsCMD = %q", got)
	}

	for _, bad := range []string{`/bin/sh -c "echo hi"`, `"just a string"`, `[1, 2]`, ``} {
		if _, err := (&Instruction{Type: InstrCMD, Args: bad, LineNum: 9}).AsCMD(); err == nil {
			t.Errorf("CMD %q should be rejected (exec form only)", bad)
		}
	}
}
