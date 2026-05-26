package forestrender

// decodeARGB splits a packed ARGB int32 into normalized RGBA components
// (0xAARRGGBB), matching ForestNote's stroke color encoding.
func decodeARGB(argb int32) (r, g, b, a float64) {
	a = float64((argb>>24)&0xFF) / 255.0
	r = float64((argb>>16)&0xFF) / 255.0
	g = float64((argb>>8)&0xFF) / 255.0
	b = float64(argb&0xFF) / 255.0
	return
}
