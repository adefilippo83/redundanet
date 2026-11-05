package backup

import "syscall"

func ctime(st *syscall.Stat_t) int64 { return st.Ctimespec.Sec*1e9 + st.Ctimespec.Nsec }
