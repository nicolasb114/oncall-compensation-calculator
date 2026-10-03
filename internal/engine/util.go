package engine

import "fmt"

func fmtSscan(s string, v *int) (int, error) { return fmt.Sscanf(s, "%d", v) }
