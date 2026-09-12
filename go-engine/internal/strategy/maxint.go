package strategy

// maxInt returns the largest of its arguments, used by the V2 strategies to express their
// warm-up requirement as one readable expression rather than a chain of if-statements.
func maxInt(vs ...int) int {
	m := 0
	for _, v := range vs {
		if v > m {
			m = v
		}
	}
	return m
}
