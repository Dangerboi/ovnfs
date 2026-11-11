package nfs

import "time"

// EvictionServer gives the external real-export integration a short lease and
// controlled clock without adding test controls to the production server API.
func EvictionServer(fs Backend, now func() time.Time) *Server {
	s := &Server{FS: fs, Concurrency: 8}
	s.v4.lease = time.Second
	s.v4.grace = time.Nanosecond
	s.v4.now = now
	return s
}
