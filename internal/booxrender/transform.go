package booxrender

// booxMatrix holds the affine transform parsed from Boox matrixValues.
// matrixValues is a 9-element array: [scaleX, skewX, transX, skewY, scaleY, transY, ...]
//
// The affine matrix is:
//
//	| scaleX skewX  transX |   x' = scaleX*x + skewX*y + transX
//	| skewY  scaleY transY |   y' = skewY*x  + scaleY*y + transY
//	| 0      0      1      |
type booxMatrix struct {
	scaleX, skewX, transX float64
	skewY, scaleY, transY float64
}

// parseMatrix extracts a booxMatrix from matrixValues. Returns nil for identity or missing data.
func parseMatrix(mv []float64) *booxMatrix {
	if len(mv) < 6 {
		return nil
	}
	if mv[0] == 1 && mv[1] == 0 && mv[2] == 0 &&
		mv[3] == 0 && mv[4] == 1 && mv[5] == 0 {
		return nil // identity
	}
	return &booxMatrix{
		scaleX: mv[0], skewX: mv[1], transX: mv[2],
		skewY: mv[3], scaleY: mv[4], transY: mv[5],
	}
}

// transformPoint applies the affine transform to a single point.
func (m *booxMatrix) transformPoint(x, y float64) (float64, float64) {
	return m.scaleX*x + m.skewX*y + m.transX,
		m.skewY*x + m.scaleY*y + m.transY
}
