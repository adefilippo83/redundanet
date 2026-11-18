package backup

import "syscall"

func ctime(st *syscall.Stat_t) int64 { return int64(st.Ctim.Sec)*1e9 + int64(st.Ctim.Nsec) }
