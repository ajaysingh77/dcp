//go:build 386 || arm

package process

import (
	"fmt"
	"math"
)

func convertPid[From ~int64 | ~int, To ~int64 | ~int | ~uint32](val From) (To, error) {
	outOfRange := val < 0 || val > math.MaxInt
	if outOfRange {
		return 0, fmt.Errorf("Value %d is out of range of valid process ID values", val)
	}
	return To(val), nil
}
