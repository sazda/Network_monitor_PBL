// Package logfile preusmerava standardni log u fajl kada program radi kao
// servis (Windows servis nema konzolu, pa bi se poruke inače izgubile).
package logfile

import (
	"io"
	"log"
	"os"
	"path/filepath"
)

const maxSize = 5 << 20

// Setup otvara dir/name za dopisivanje i postavlja ga kao izlaz standardnog
// loga. Ako je fajl veći od 5 MB, prethodni se čuva kao name.old.
func Setup(dir, name string) (io.Closer, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxSize {
		os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	log.SetOutput(f)
	return f, nil
}
