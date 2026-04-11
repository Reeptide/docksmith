package builder

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
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
		case InstrFROM, InstrCOPY, InstrRUN, InstrWORKDIR, InstrENV, InstrCMD:
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
