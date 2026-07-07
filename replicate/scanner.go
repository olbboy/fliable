package replicate

import (
	"bufio"
	"io"
)

// newLineScanner returns a line scanner sized for large journal entries
// (instances with big variable maps serialize to long lines).
func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	return sc
}
