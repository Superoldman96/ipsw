package dyld

import (
	"encoding/binary"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/apex/log"
	"github.com/blacktop/go-arm64"
	"github.com/blacktop/go-macho"
	"github.com/blacktop/go-macho/types"
	"github.com/blacktop/ipsw/internal/demangle"
	"github.com/blacktop/ipsw/internal/utils"
)

// GetSymbolAddress returns the virtual address and possibly the dylib containing a given symbol
func (f *File) GetSymbolAddress(symbol, imageName string) (uint64, *CacheImage, error) {
	if len(imageName) > 0 {
		if sym, _ := f.FindExportedSymbolInImage(imageName, symbol); sym != nil {
			return sym.Address, f.Image(imageName), nil
		}
	} else {
		// Search ALL dylibs for the symbol
		for _, image := range f.Images {
			if sym, _ := f.FindExportedSymbolInImage(image.Name, symbol); sym != nil {
				return sym.Address, image, nil
			}
		}
	}

	// Search addr2sym map
	for addr, sym := range f.AddressToSymbol {
		if strings.EqualFold(sym, symbol) {
			return addr, nil, nil
		}
	}

	return 0, nil, fmt.Errorf("failed to find symbol %s", symbol)
}

func (f *File) FirstPass(r io.ReadSeeker, options arm64.Options) []uint64 {
	var addrs []uint64

	var prevInstruction arm64.Instruction

	for i := range arm64.Disassemble(r, options) {

		if i.Error != nil {
			fmt.Println(i.StrRepr)
			continue
		}

		// lookup adrp/ldr or add address as a cstring or symbol name
		operation := i.Instruction.Operation().String()
		if (operation == "ldr" || operation == "add") && prevInstruction.Operation().String() == "adrp" {
			operands := i.Instruction.Operands()
			if operands != nil && prevInstruction.Operands() != nil {
				adrpRegister := prevInstruction.Operands()[0].Reg[0]
				adrpImm := prevInstruction.Operands()[1].Immediate
				if operation == "ldr" && adrpRegister == operands[1].Reg[0] {
					adrpImm += operands[1].Immediate
				} else if operation == "add" && adrpRegister == operands[1].Reg[0] {
					adrpImm += operands[2].Immediate
				}
				addrs = append(addrs, adrpImm)
			}

		} else if i.Instruction.Group() == arm64.GROUP_BRANCH_EXCEPTION_SYSTEM { // check if branch location is a function
			operands := i.Instruction.Operands()
			if operands != nil && operands[0].OpClass == arm64.LABEL {
				addrs = append(addrs, operands[0].Immediate)
			}
		} else if i.Instruction.Group() == arm64.GROUP_DATA_PROCESSING_IMM || i.Instruction.Group() == arm64.GROUP_LOAD_STORE {
			operation := i.Instruction.Operation()
			if operation == arm64.ARM64_LDR || operation == arm64.ARM64_ADR {
				operands := i.Instruction.Operands()
				if operands[1].OpClass == arm64.LABEL {
					addrs = append(addrs, operands[1].Immediate)
				}
			}
		}

		prevInstruction = *i.Instruction
	}
	return addrs
}

// ImageDependencies recursively returns all the image's loaded dylibs and those dylibs' loaded dylibs etc
func (f *File) ImageDependencies(imageName string) error {

	image := f.Image(imageName)

	m, err := image.GetPartialMacho()
	if err != nil {
		return err
	}
	defer m.Close()

	imports := m.ImportedLibraries()
	if len(imports) == 0 {
		return nil
	}

	for _, imp := range imports {
		if !utils.StrSliceContains(image.Analysis.Dependencies, imp) {
			image.Analysis.Dependencies = append(image.Analysis.Dependencies, imp)
			if err := f.ImageDependencies(imp); err != nil {
				return err
			}
			image.Analysis.Dependencies = utils.Unique(image.Analysis.Dependencies)
		}
	}

	return nil
}

// IsFunctionStart checks if address is at a function start and returns symbol name
func (f *File) IsFunctionStart(funcs []types.Function, addr uint64, shouldDemangle bool) (bool, string) {
	for _, fn := range funcs {
		if addr == fn.StartAddr {
			if symName, ok := f.AddressToSymbol[addr]; ok {
				if shouldDemangle {
					return ok, demangle.Do(symName)
				}
				return ok, symName
			}
			return true, ""
		}
	}
	return false, ""
}

// FindSymbol returns symbol from the addr2symbol map for a given virtual address
func (f *File) FindSymbol(addr uint64, shouldDemangle bool) string {
	if symName, ok := f.AddressToSymbol[addr]; ok {
		if shouldDemangle {
			return demangle.Do(symName)
		}
		return symName
	}

	return ""
}

// IsCString returns cstring at given virtual address if is in a CstringLiterals section
func (f *File) IsCString(m *macho.File, addr uint64) (string, error) {
	for _, sec := range m.Sections {
		if sec.Flags.IsCstringLiterals() {
			if sec.Addr <= addr && addr < sec.Addr+sec.Size {
				return f.GetCString(addr)
			}
		}
	}
	return "", fmt.Errorf("not a cstring address")
}

// AnalyzeImage analyzes an image by parsing it's symbols, stubs and GOT
func (f *File) AnalyzeImage(image *CacheImage) error {

	if err := f.GetAllExportedSymbolsForImage(image, false); err != nil {
		log.Errorf("failed to parse exported symbols for %s", image.Name)
	}

	if err := f.GetLocalSymbolsForImage(image); err != nil {
		log.Errorf("failed to parse local symbols for %s", image.Name)
	}

	if !image.Analysis.State.IsStubsDone() {
		log.Debugf("parsing %s symbol stubs", image.Name)
		if err := f.ParseSymbolStubs(image); err != nil {
			return err
		}

		for stub, target := range image.Analysis.SymbolStubs {
			if symName, ok := f.AddressToSymbol[target]; ok {
				f.AddressToSymbol[stub] = fmt.Sprintf("j_%s", symName)
			} else {
				img, err := f.GetImageContainingTextAddr(target)
				if err != nil {
					return err
				}
				if err := f.AnalyzeImage(img); err != nil {
					return err
				}
				if symName, ok := f.AddressToSymbol[target]; ok {
					f.AddressToSymbol[stub] = fmt.Sprintf("j_%s", symName)
				} else {
					f.AddressToSymbol[stub] = fmt.Sprintf("__stub_%x ; %s", target, filepath.Base(img.Name))
					log.Errorf("no sym found for __stub: %#x; found in %s", stub, img.Name)
				}
			}
		}
	}

	if !image.Analysis.State.IsGotDone() {
		log.Debugf("parsing %s global offset table", image.Name)
		if err := f.ParseGOT(image); err != nil {
			return err
		}

		for entry, target := range image.Analysis.GotPointers {
			if symName, ok := f.AddressToSymbol[target]; ok {
				f.AddressToSymbol[entry] = fmt.Sprintf("__got.%s", symName)
			} else {
				if img, err := f.GetImageContainingTextAddr(target); err == nil {
					if err := f.AnalyzeImage(img); err != nil {
						return err
					}
					if symName, ok := f.AddressToSymbol[target]; ok {
						f.AddressToSymbol[entry] = fmt.Sprintf("__got.%s", symName)
					} else {
						// log.Errorf("no sym found for __got: %#x; found in %s", entry, img.Name)
						f.AddressToSymbol[entry] = fmt.Sprintf("__got_%x ; %s", target, filepath.Base(img.Name))
					}
				} else {
					f.AddressToSymbol[entry] = fmt.Sprintf("__got_%x", target)
				}

			}
		}
	}

	return nil
}

// ParseSymbolStubs parse symbol stubs in MachO
func (f *File) ParseSymbolStubs(image *CacheImage) error {

	m, err := image.GetPartialMacho()
	if err != nil {
		return fmt.Errorf("failed to get MachO for image %s; %#v", image.Name, err)
	}
	defer m.Close()

	image.Analysis.SymbolStubs = make(map[uint64]uint64)

	for _, sec := range m.Sections {
		if sec.Flags.IsSymbolStubs() {

			var adrpImm uint64
			var adrpAddr uint64
			var prevInst arm64.Instruction

			offset, err := f.GetOffset(sec.Addr)
			if err != nil {
				return err
			}
			r := io.NewSectionReader(f.r, int64(offset), int64(sec.Size))

			for i := range arm64.Disassemble(r, arm64.Options{StartAddress: int64(sec.Addr)}) {
				if i.Instruction.Operation() == arm64.ARM64_ADD && prevInst.Operation() == arm64.ARM64_ADRP {
					if i.Instruction.Operands() != nil && prevInst.Operands() != nil {
						// adrp      	x17, #0x1e3be9000
						adrpRegister := prevInst.Operands()[0].Reg[0] // x17
						adrpImm = prevInst.Operands()[1].Immediate    // #0x1e3be9000
						// add       	x17, x17, #0x1c0
						if adrpRegister == i.Instruction.Operands()[0].Reg[0] {
							adrpImm += i.Instruction.Operands()[2].Immediate
							adrpAddr = prevInst.Address()
						}
					}
				} else if i.Instruction.Operation() == arm64.ARM64_LDR && prevInst.Operation() == arm64.ARM64_ADD {
					// add       	x17, x17, #0x1c0
					addRegister := prevInst.Operands()[0].Reg[0] // x17
					// ldr       	x16, [x17]
					if addRegister == i.Instruction.Operands()[1].Reg[0] {
						addr, err := f.ReadPointerAtAddress(adrpImm)
						if err != nil {
							return fmt.Errorf("failed to read pointer at %#x: %#v", adrpImm, err)
						}
						image.Analysis.SymbolStubs[adrpAddr] = f.SlideInfo.SlidePointer(addr)
					}
				} else if i.Instruction.Operation() == arm64.ARM64_BR && prevInst.Operation() == arm64.ARM64_ADD {
					// add       	x16, x16, #0x828
					addRegister := prevInst.Operands()[0].Reg[0] // x16
					// br        	x16
					if addRegister == i.Instruction.Operands()[0].Reg[0] {
						image.Analysis.SymbolStubs[adrpAddr] = adrpImm
					}
				}

				// fmt.Printf("%#08x:  %s\t%-10v%s\n", i.Instruction.Address(), i.Instruction.OpCodes(), i.Instruction.Operation(), i.Instruction.OpStr())
				prevInst = *i.Instruction
			}
		}
	}

	image.Analysis.State.SetStubs(true)

	return nil
}

// ParseGOT parse global offset table in MachO
func (f *File) ParseGOT(image *CacheImage) error {

	m, err := image.GetPartialMacho()
	if err != nil {
		return fmt.Errorf("failed to get MachO for image %s; %#v", image.Name, err)
	}
	defer m.Close()

	image.Analysis.GotPointers = make(map[uint64]uint64)

	if authPtr := m.Section("__AUTH_CONST", "__auth_ptr"); authPtr != nil {
		r := io.NewSectionReader(f.r, int64(authPtr.Offset), int64(authPtr.Size))

		ptrs := make([]uint64, authPtr.Size/8)
		if err := binary.Read(r, binary.LittleEndian, &ptrs); err != nil {
			return fmt.Errorf("failed to read __AUTH_CONST.__auth_ptr ptrs; %#v", err)
		}

		for idx, ptr := range ptrs {
			image.Analysis.GotPointers[authPtr.Addr+uint64(idx*8)] = f.SlideInfo.SlidePointer(ptr)
		}
	}

	for _, sec := range m.Sections {
		if sec.Flags.IsNonLazySymbolPointers() {
			r := io.NewSectionReader(f.r, int64(sec.Offset), int64(sec.Size))

			ptrs := make([]uint64, sec.Size/8)
			if err := binary.Read(r, binary.LittleEndian, &ptrs); err != nil {
				return fmt.Errorf("failed to read %s.%s NonLazySymbol pointers; %#v", sec.Seg, sec.Name, err)
			}

			for idx, ptr := range ptrs {
				image.Analysis.GotPointers[sec.Addr+uint64(idx*8)] = f.SlideInfo.SlidePointer(ptr)
			}
		}
	}

	image.Analysis.State.SetGot(true)

	return nil
}
