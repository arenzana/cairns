// Package pca projects high-dimensional embeddings down to two dimensions so
// the corpus can be drawn.
//
// PCA rather than UMAP or t-SNE, deliberately. Those give prettier separation
// but need a real numerical stack, are non-deterministic run to run, and their
// distances are not interpretable. PCA is ~80 lines, deterministic, and the
// axes mean something: they are the two directions along which this corpus
// varies most. For "does my library actually cluster", that is the honest
// picture.
package pca

import "math"

// Model is a fitted projection: subtract the mean, then dot with two axes.
type Model struct {
	Mean []float32
	PC1  []float32
	PC2  []float32
	PC3  []float32
}

// Fit finds the two leading principal components by power iteration.
//
// The covariance matrix is never formed. At 1,024 dimensions it would be a
// million entries and cost n*d² to build; instead each iteration applies
// X^T(Xv), which is 2*n*d. For 1,391 documents that is about 3M multiplies per
// iteration rather than 1.4B to build the matrix once.
func Fit(rows [][]float32, iters int) *Model {
	if len(rows) == 0 || len(rows[0]) == 0 {
		return nil
	}
	d := len(rows[0])

	mean := make([]float32, d)
	for _, r := range rows {
		for j, v := range r {
			mean[j] += v
		}
	}
	for j := range mean {
		mean[j] /= float32(len(rows))
	}

	centered := make([][]float32, len(rows))
	for i, r := range rows {
		c := make([]float32, d)
		for j, v := range r {
			c[j] = v - mean[j]
		}
		centered[i] = c
	}

	pc1 := power(centered, nil, iters)
	// Deflate: strip the first component out of every row, then the leading
	// direction of what remains is the second component, orthogonal by
	// construction. Repeat for the third.
	pc2 := power(centered, pc1, iters)
	pc3 := power(centered, pc1, iters, pc2)

	return &Model{Mean: mean, PC1: pc1, PC2: pc2, PC3: pc3}
}

// power runs power iteration, optionally with one prior component projected
// out of both the data and the running estimate.
func power(rows [][]float32, deflate []float32, iters int, more ...[]float32) []float32 {
	d := len(rows[0])

	// Deterministic seed rather than random: the same corpus must produce the
	// same map every time, or the picture appears to move when nothing changed.
	v := make([]float32, d)
	for i := range v {
		v[i] = float32(math.Sin(float64(i)*0.7 + 1))
	}
	normalize(v)

	scratch := make([]float32, d)
	for range iters {
		for i := range scratch {
			scratch[i] = 0
		}
		for _, r := range rows {
			dot := dotDeflated(r, v, deflate)
			if dot == 0 {
				continue
			}
			addScaledDeflated(scratch, r, dot, deflate)
		}
		if deflate != nil {
			orthogonalize(scratch, deflate)
		}
		for _, b := range more {
			orthogonalize(scratch, b)
		}
		if normalize(scratch) == 0 {
			break
		}
		copy(v, scratch)
	}
	return v
}

// dotDeflated computes r·v with the deflated component removed from r.
func dotDeflated(r, v, deflate []float32) float32 {
	if deflate == nil {
		return dot(r, v)
	}
	proj := dot(r, deflate)
	var s float32
	for i := range r {
		s += (r[i] - proj*deflate[i]) * v[i]
	}
	return s
}

func addScaledDeflated(dst, r []float32, scale float32, deflate []float32) {
	if deflate == nil {
		for i := range r {
			dst[i] += scale * r[i]
		}
		return
	}
	proj := dot(r, deflate)
	for i := range r {
		dst[i] += scale * (r[i] - proj*deflate[i])
	}
}

func orthogonalize(v, basis []float32) {
	p := dot(v, basis)
	for i := range v {
		v[i] -= p * basis[i]
	}
}

func dot(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func normalize(v []float32) float32 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	n := float32(math.Sqrt(s))
	if n == 0 {
		return 0
	}
	for i := range v {
		v[i] /= n
	}
	return n
}

// Project maps one vector onto the three components.
func (m *Model) Project(v []float32) (x, y, z float32) {
	if m == nil || len(v) != len(m.Mean) {
		return 0, 0, 0
	}
	for i, val := range v {
		c := val - m.Mean[i]
		x += c * m.PC1[i]
		y += c * m.PC2[i]
		z += c * m.PC3[i]
	}
	return x, y, z
}

// Variance reports how much of the corpus spread each component captures,
// which is the honest caption for a projection: three axes out of 1,024 is a
// shadow, and this number says how much of the shape survived.
func (m *Model) Variance(rows [][]float32) (v1, v2, v3, total float64) {
	for _, r := range rows {
		x, y, z := m.Project(r)
		v1 += float64(x) * float64(x)
		v2 += float64(y) * float64(y)
		v3 += float64(z) * float64(z)
		for i, val := range r {
			c := float64(val - m.Mean[i])
			total += c * c
		}
	}
	return
}
