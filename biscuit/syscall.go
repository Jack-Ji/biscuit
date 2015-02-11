package main

import "fmt"
import "runtime"
//import "sync"
import "unsafe"

const TFSIZE       int = 23
const TFREGS       int = 16
const TF_R8        int = 8
const TF_RSI       int = 10
const TF_RDI       int = 11
const TF_RDX       int = 12
const TF_RCX       int = 13
const TF_RBX       int = 14
const TF_RAX       int = 15
const TF_TRAP      int = TFREGS
const TF_RIP       int = TFREGS + 2
const TF_RSP       int = TFREGS + 5
const TF_RFLAGS    int = TFREGS + 4

const EFAULT       int = 14
const ENOSYS       int = 38

const SYS_WRITE      int = 1
const SYS_GETPID     int = 39
const SYS_FORK       int = 57
const SYS_EXIT       int = 60

// lowest userspace address
const USERMIN      int = 0xf1000000

func syscall(pid int, tf *[TFSIZE]int) {

	p := proc_get(pid)
	trap := tf[TF_RAX]
	a1 := tf[TF_RDI]
	a2 := tf[TF_RSI]
	a3 := tf[TF_RDX]
	//a4 := tf[TF_RCX]
	//a5 := tf[TF_R8]

	ret := -ENOSYS
	switch trap {
	case SYS_WRITE:
		ret = sys_write(p, a1, a2, a3)
	case SYS_GETPID:
		ret = sys_getpid(p)
	case SYS_FORK:
		ret = sys_fork(p, tf)
	case SYS_EXIT:
		sys_exit(p, a1)
	}

	tf[TF_RAX] = ret
	if !p.dead {
		runtime.Procrunnable(pid, tf)
	}
}

func sys_write(proc *proc_t, fd int, bufp int, c int) int {

	if c == 0 {
		return 0
	}

	if !is_mapped(proc.pmap, bufp, c) {
		fmt.Printf("%#x not mapped\n", bufp)
		return -EFAULT
	}

	if fd != 1 && fd != 2 {
		panic("no imp")
	}

	vtop := func(va int) int {
		pte := pmap_walk(proc.pmap, va, false, 0, nil)
		ret := *pte & PTE_ADDR
		ret += va & PGOFFSET
		return ret
	}

	utext := int8(0x17)
	cnt := 0
	for ; cnt < c; {
		p_bufp := vtop(bufp + cnt)
		p := dmap8(p_bufp)
		left := c - cnt
		if len(p) > left {
			p = p[:left]
		}
		for _, c := range p {
			runtime.Putcha(int8(c), utext)
		}
		cnt += len(p)
	}
	return c
}

func sys_getpid(proc *proc_t) int {
	return proc.pid
}

func sys_fork(parent *proc_t, ptf *[TFSIZE]int) int {

	child := proc_new(fmt.Sprintf("%s's child", parent.name))

	// mark writable entries as read-only and cow
	mk_cow := func(pte int) (int, int) {

		// don't mess with or track kernel pages
		if pte & PTE_U == 0 {
			return pte, pte
		}
		if pte & PTE_W != 0 {
			pte = (pte | PTE_COW) & ^PTE_W
		}

		// reference the mapped page in child's tracking maps too
		p_pg := pte & PTE_ADDR
		pg, ok := parent.pages[p_pg]
		if !ok {
			panic(fmt.Sprintf("parent not tracking " +
			    "page %#x", p_pg))
		}
		upg, ok := parent.upages[p_pg]
		if !ok {
			panic(fmt.Sprintf("parent not tracking " +
			    "upage %#x", p_pg))
		}
		child.pages[p_pg] = pg
		child.upages[p_pg] = upg
		return pte, pte
	}
	pmap, p_pmap, _ := copy_pmap(mk_cow, parent.pmap, child.pages)

	// tlb invalidation is not necessary for the parent because its pmap
	// cannot be in use now (well, it may be in use on the CPU that took
	// the syscall interrupt which may not have switched pmaps yet, but
	// that CPU will not touch these invalidated user-space addresses).

	child.pmap = pmap
	child.p_pmap = p_pmap

	chtf := [TFSIZE]int{}
	chtf = *ptf
	chtf[TF_RAX] = 0

	child.sched_add(&chtf)

	return child.pid
}

func sys_pgfault(proc *proc_t, pte *int, faultaddr int, tf *[TFSIZE]int) {
	// copy page
	dst, p_dst := pg_new(proc.pages)
	p_src := *pte & PTE_ADDR
	src := dmap(p_src)
	for i, c := range src {
		dst[i] = c
	}

	// insert new page into pmap
	va := faultaddr & PGMASK
	perms := (*pte & PTE_FLAGS) & ^PTE_COW
	perms |= PTE_W
	proc.page_insert(va, dst, p_dst, perms, false)

	// set process as runnable again
	runtime.Procrunnable(proc.pid, nil)
}

func sys_exit(proc *proc_t, status int) {
	fmt.Printf("%v exited with status %v\n", proc.name, status)
	proc_kill(proc.pid)
}

type elf_t struct {
	data	[]uint8
	len	int
}

type elf_phdr struct {
	etype   int
	flags   int
	vaddr   int
	filesz  int
	memsz   int
	sdata    []uint8
}

var ELF_QUARTER = 2
var ELF_HALF = 4
var ELF_OFF = 8
var ELF_ADDR = 8
var ELF_XWORD = 8

func readn(a []uint8, n int, off int) int {
	ret := 0
	for i := 0; i < n; i++ {
		ret |= int(a[off + i]) << uint(8*i)
	}
	return ret
}

func (e *elf_t) npheaders() int {
	mag := readn(e.data, ELF_HALF, 0)
	if mag != 0x464c457f {
		panic("bad elf magic")
	}
	e_phnum := 0x38
	return readn(e.data, ELF_QUARTER, e_phnum)
}

func (e *elf_t) header(c int, ret *elf_phdr) {
	if ret == nil {
		panic("nil elf_t")
	}

	nph := e.npheaders()
	if c >= nph {
		panic("bad elf header")
	}
	d := e.data
	e_phoff := 0x20
	e_phentsize := 0x36
	hoff := readn(d, ELF_OFF, e_phoff)
	hsz  := readn(d, ELF_QUARTER, e_phentsize)

	p_type   := 0x0
	p_flags  := 0x4
	p_offset := 0x8
	p_vaddr  := 0x10
	p_filesz := 0x20
	p_memsz  := 0x28
	f := func(w int, sz int) int {
		return readn(d, sz, hoff + c*hsz + w)
	}
	ret.etype = f(p_type, ELF_HALF)
	ret.flags = f(p_flags, ELF_HALF)
	ret.vaddr = f(p_vaddr, ELF_ADDR)
	ret.filesz = f(p_filesz, ELF_XWORD)
	ret.memsz = f(p_memsz, ELF_XWORD)
	off := f(p_offset, ELF_OFF)
	if off < 0 || off >= len(d) {
		panic(fmt.Sprintf("weird off %v", off))
	}
	if ret.filesz < 0 || off + ret.filesz >= len(d) {
		panic(fmt.Sprintf("weird filesz %v", ret.filesz))
	}
	rd := d[off:off + ret.filesz]
	ret.sdata = rd
}

func (e *elf_t) headers() []elf_phdr {
	num := e.npheaders()
	ret := make([]elf_phdr, num)
	for i := 0; i < num; i++ {
		e.header(i, &ret[i])
	}
	return ret
}

func (e *elf_t) entry() int {
	e_entry := 0x18
	return readn(e.data, ELF_ADDR, e_entry)
}

func elf_segload(p *proc_t, hdr *elf_phdr) {
	perms := PTE_U
	//PF_X := 1
	PF_W := 2
	if hdr.flags & PF_W != 0 {
		perms |= PTE_W
	}
	sz := roundup(hdr.vaddr + hdr.memsz, PGSIZE)
	sz -= rounddown(hdr.vaddr, PGSIZE)
	rsz := hdr.filesz
	for i := 0; i < sz; i += PGSIZE {
		// go allocator zeros all pages for us, thus bss is already
		// initialized
		pg, p_pg := pg_new(p.pages)
		//pg, p_pg := pg_new(&p.pages)
		if i < len(hdr.sdata) {
			dst := unsafe.Pointer(pg)
			src := unsafe.Pointer(&hdr.sdata[i])
			len := PGSIZE
			left := rsz - i
			if len > left {
				len = left
			}
			runtime.Memmove(dst, src, len)
		}
		p.page_insert(hdr.vaddr + i, pg, p_pg, perms, true)
	}
}

func elf_load(p *proc_t, e *elf_t) {
	PT_LOAD := 1
	for _, hdr := range e.headers() {
		// XXX get rid of worthless user program segments
		if hdr.etype == PT_LOAD && hdr.vaddr >= USERMIN {
			elf_segload(p, &hdr)
		}
	}
}

func sys_test_dump() {
	e := allbins["user/hello"]
	for i, hdr := range e.headers() {
		fmt.Printf("%v -- vaddr %x filesz %x ", i, hdr.vaddr, hdr.filesz)
	}
}

func sys_test(program string) {
	fmt.Printf("add 'user' prog\n")

	var tf [23]int
	tfregs    := 16
	tf_rsp    := tfregs + 5
	tf_rip    := tfregs + 2
	tf_rflags := tfregs + 4
	fl_intf   := 1 << 9
	tf_ss     := tfregs + 6
	tf_cs     := tfregs + 3

	proc := proc_new(program + "test")

	elf, ok := allbins[program]
	if !ok {
		panic("no such program: " + program)
	}

	stack, p_stack := pg_new(proc.pages)
	stackva := 0xf4000000
	tf[tf_rsp] = stackva - 8
	tf[tf_rip] = elf.entry()
	tf[tf_rflags] = fl_intf

	ucseg := 4
	udseg := 5
	tf[tf_cs] = ucseg << 3 | 3
	tf[tf_ss] = udseg << 3 | 3

	// copy kernel page table, map new stack
	upmap, p_upmap, _ := copy_pmap(nil, kpmap(), proc.pages)
	proc.pmap, proc.p_pmap = upmap, p_upmap
	proc.page_insert(stackva - PGSIZE, stack,
	    p_stack, PTE_U | PTE_W, true)

	elf_load(proc, elf)

	// since kernel and user programs share pml4[0], need to mark shared
	// pages user
	pmap_cperms(upmap, elf.entry(), PTE_U)
	pmap_cperms(upmap, stackva, PTE_U)

	proc.sched_add(&tf)
}