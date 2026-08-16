package builder

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type InstructionType string

const (
	InstrFROM    InstructionType = "FROM"
	InstrCOPY    InstructionType = "COPY"
	InstrRUN     InstructionType = "RUN"
	InstrWORKDIR InstructionType = "WORKDIR"
	InstrENV     InstructionType = "ENV"
	InstrCMD     InstructionType = "CMD"
	InstrEXPOSE  InstructionType = "EXPOSE"
)

type Instruction struct {
	LineNum int
	Type    InstructionType
	Args    string // raw args string
}

// ParsedFROM holds FROM args.
type ParsedFROM struct {
	Name string
	Tag  string
}

// ParsedCOPY holds COPY args.
type ParsedCOPY struct {
	Src  string
	Dest string
}

// ParsedENV holds ENV key=value.
type ParsedENV struct {
	Key   string
	Value string
}

func ParseFile(path string) ([]Instruction, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open Docksmithfile: %w", err)
	}
	defer f.Close()

	var instrs []Instruction
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		keyword := strings.ToUpper(parts[0])
		args := ""
		if len(parts) == 2 {
			args = strings.TrimSpace(parts[1])
		}
		switch InstructionType(keyword) {
		case InstrFROM, InstrCOPY, InstrRUN, InstrWORKDIR, InstrENV, InstrCMD, InstrEXPOSE:
			instrs = append(instrs, Instruction{
				LineNum: lineNum,
				Type:    InstructionType(keyword),
				Args:    args,
			})
		default:
			return nil, fmt.Errorf("line %d: unrecognised instruction %q", lineNum, keyword)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return instrs, nil
}

func (i *Instruction) AsFROM() (*ParsedFROM, error) {
	parts := strings.SplitN(i.Args, ":", 2)
	name := parts[0]
	tag := "latest"
	if len(parts) == 2 {
		tag = parts[1]
	}
	return &ParsedFROM{Name: name, Tag: tag}, nil
}

func (i *Instruction) AsCOPY() (*ParsedCOPY, error) {
	parts := strings.Fields(i.Args)
	if len(parts) < 2 {
		return nil, fmt.Errorf("line %d: COPY requires <src> <dest>", i.LineNum)
	}
	return &ParsedCOPY{Src: parts[0], Dest: parts[1]}, nil
}

func (i *Instruction) AsENV() (*ParsedENV, error) {
	idx := strings.Index(i.Args, "=")
	if idx < 0 {
		return nil, fmt.Errorf("line %d: ENV requires KEY=VALUE", i.LineNum)
	}
	return &ParsedENV{Key: i.Args[:idx], Value: i.Args[idx+1:]}, nil
}

func (i *Instruction) AsCMD() ([]string, error) {
	var cmd []string
	if err := json.Unmarshal([]byte(i.Args), &cmd); err != nil {
		return nil, fmt.Errorf("line %d: CMD must be a JSON array: %w", i.LineNum, err)
	}
	return cmd, nil
}

// AsEXPOSE parses "EXPOSE 80 443/udp" into normalised port/protocol strings.
//
// Like ENV, WORKDIR and CMD, EXPOSE produces no layer. It also stays out of
// cache keys: it cannot change a single byte of any layer, so including it
// would invalidate every existing cache entry on upgrade and buy nothing.
func (i *Instruction) AsEXPOSE() ([]string, error) {
	fields := strings.Fields(i.Args)
	if len(fields) == 0 {
		return nil, fmt.Errorf("line %d: EXPOSE requires at least one port", i.LineNum)
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		port, proto := f, "tcp"
		if base, p, found := strings.Cut(f, "/"); found {
			port, proto = base, strings.ToLower(p)
			if proto != "tcp" && proto != "udp" {
				return nil, fmt.Errorf("line %d: EXPOSE %q: protocol must be tcp or udp", i.LineNum, f)
			}
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("line %d: EXPOSE %q: invalid port", i.LineNum, f)
		}
		out = append(out, fmt.Sprintf("%d/%s", n, proto))
	}
	return out, nil
}
