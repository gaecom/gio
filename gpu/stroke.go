// SPDX-License-Identifier: Unlicense OR MIT

// Most of the algorithms to compute strokes and their offsets have been
// extracted, adapted from (and used as a reference implementation):
//  - github.com/tdewolff/canvas (Licensed under MIT)
//
// These algorithms have been implemented from:
//  Fast, precise flattening of cubic Bézier path and offset curves
//   Thomas F. Hain, et al.
//
// An electronic version is available at:
//  https://seant23.files.wordpress.com/2010/11/fastpreciseflatteningofbeziercurve.pdf
//
// Possible improvements (in term of speed and/or accuracy) on these
// algorithms are:
//
//  - Polar Stroking: New Theory and Methods for Stroking Paths,
//    M. Kilgard
//    https://arxiv.org/pdf/2007.00308.pdf
//
//  - https://raphlinus.github.io/graphics/curves/2019/12/23/flatten-quadbez.html
//    R. Levien

package gpu

import (
	"math"

	"gioui.org/f32"
	"gioui.org/internal/ops"
	"gioui.org/op"
	"gioui.org/op/clip"
)

type strokeQuad struct {
	contour uint32
	quad    ops.Quad
}

type strokeState struct {
	p0, p1 f32.Point // p0 is the start point, p1 the end point.
	n0, n1 f32.Point // n0 is the normal vector at the start point, n1 at the end point.
	r0, r1 float32   // r0 is the curvature at the start point, r1 at the end point.
	ctl    f32.Point // ctl is the control point of the quadratic Bézier segment.
}

type strokeQuads []strokeQuad

func (qs *strokeQuads) pen() f32.Point {
	return (*qs)[len(*qs)-1].quad.To
}

func (qs *strokeQuads) lineTo(pt f32.Point) {
	end := (*qs)[len(*qs)-1].quad.To
	*qs = append(*qs, strokeQuad{
		quad: ops.Quad{
			From: end,
			Ctrl: end.Add(pt).Mul(0.5),
			To:   pt,
		},
	})
}

func (qs *strokeQuads) arc(f1, f2 f32.Point, angle float32) {
	var (
		p clip.Path
		o = new(op.Ops)
	)
	p.Begin(o)
	p.Move(qs.pen())
	beg := len(o.Data())
	p.Arc(f1, f2, angle)
	end := len(o.Data())
	raw := o.Data()[beg:end]

	for qi := 0; len(raw) >= (ops.QuadSize + 4); qi++ {
		quad := ops.DecodeQuad(raw[4:])
		raw = raw[ops.QuadSize+4:]
		*qs = append(*qs, strokeQuad{
			quad: quad,
		})
	}
}

// split splits a slice of quads into slices of quads grouped
// by contours (ie: splitted at move-to boundaries).
func (qs strokeQuads) split() []strokeQuads {
	if len(qs) == 0 {
		return nil
	}

	var (
		c uint32
		o []strokeQuads
		i = len(o)
	)
	for _, q := range qs {
		if q.contour != c {
			c = q.contour
			i = len(o)
			o = append(o, strokeQuads{})
		}
		o[i] = append(o[i], q)
	}

	return o
}

func (qs strokeQuads) stroke(width float32, sty clip.StrokeStyle) strokeQuads {
	var (
		o  strokeQuads
		hw = 0.5 * width
	)

	for _, ps := range qs.split() {
		rhs, lhs := ps.offset(hw, sty)
		switch lhs {
		case nil:
			o = o.append(rhs)
		default:
			// Closed path.
			// Inner path should go opposite direction to cancel outer path.
			switch {
			case ps.ccw():
				lhs = lhs.reverse()
				o = o.append(rhs)
				o = o.append(lhs)
			default:
				rhs = rhs.reverse()
				o = o.append(lhs)
				o = o.append(rhs)
			}
		}
	}
	return o
}

// offset returns the right-hand and left-hand sides of the path, offset by
// the half-width hw.
// The stroke style sty handles how segments are joined and ends are capped.
func (qs strokeQuads) offset(hw float32, sty clip.StrokeStyle) (rhs, lhs strokeQuads) {
	var (
		states []strokeState
		beg    = qs[0].quad.From
		end    = qs[len(qs)-1].quad.To
		closed = beg == end
	)
	for i := range qs {
		q := qs[i].quad

		var (
			n0 = strokePathNorm(q.From, q.Ctrl, q.To, 0, hw)
			n1 = strokePathNorm(q.From, q.Ctrl, q.To, 1, hw)
			r0 = strokePathCurv(q.From, q.Ctrl, q.To, 0)
			r1 = strokePathCurv(q.From, q.Ctrl, q.To, 1)
		)
		states = append(states, strokeState{
			p0:  q.From,
			p1:  q.To,
			n0:  n0,
			n1:  n1,
			r0:  r0,
			r1:  r1,
			ctl: q.Ctrl,
		})
	}

	const tolerance = 0.01
	for i, state := range states {
		rhs = rhs.append(strokeQuadBezier(state, +hw, tolerance))
		lhs = lhs.append(strokeQuadBezier(state, -hw, tolerance))

		// join the current and next segments
		if hasNext := i+1 < len(states); hasNext || closed {
			var next strokeState
			switch {
			case hasNext:
				next = states[i+1]
			case closed:
				next = states[0]
			}
			if state.n1 != next.n0 {
				strokePathJoin(sty, &rhs, &lhs, hw, state.p1, state.n1, next.n0, state.r1, next.r0)
			}
		}
	}

	if closed {
		rhs.close()
		lhs.close()
		return rhs, lhs
	}

	qbeg := &states[0]
	qend := &states[len(states)-1]

	// Default to counter-clockwise direction.
	lhs = lhs.reverse()
	strokePathCap(sty, &rhs, hw, qend.p1, qend.n1)

	rhs = rhs.append(lhs)
	strokePathCap(sty, &rhs, hw, qbeg.p0, qbeg.n0.Mul(-1))

	rhs.close()

	return rhs, nil
}

func (qs *strokeQuads) close() {
}

// ccw returns whether the path is counter-clockwise.
func (qs strokeQuads) ccw() bool {
	// Use the Shoelace formula:
	//  https://en.wikipedia.org/wiki/Shoelace_formula
	var area float32
	for _, ps := range qs.split() {
		for i := 1; i < len(ps); i++ {
			pi := ps[i].quad.To
			pj := ps[i-1].quad.To
			area += (pi.X - pj.X) * (pi.Y + pj.Y)
		}
	}
	return area <= 0.0
}

func (qs strokeQuads) reverse() strokeQuads {
	if len(qs) == 0 {
		return nil
	}

	ps := make(strokeQuads, 0, len(qs))
	for i := range qs {
		q := qs[len(qs)-1-i]
		q.quad.To, q.quad.From = q.quad.From, q.quad.To
		ps = append(ps, q)
	}

	return ps
}

func (qs strokeQuads) append(ps strokeQuads) strokeQuads {
	switch {
	case len(ps) == 0:
		return qs
	case len(qs) == 0:
		return ps
	}
	return append(qs, ps...)
}

// strokePathNorm returns the normal vector at t.
func strokePathNorm(p0, p1, p2 f32.Point, t, d float32) f32.Point {
	switch t {
	case 0:
		n := p1.Sub(p0)
		if n.X == 0 && n.Y == 0 {
			return f32.Point{}
		}
		n = rot90CW(n)
		return normPt(n, d)
	case 1:
		n := p2.Sub(p1)
		if n.X == 0 && n.Y == 0 {
			return f32.Point{}
		}
		n = rot90CW(n)
		return normPt(n, d)
	}
	panic("impossible")
}

func rot90CW(p f32.Point) f32.Point  { return f32.Pt(+p.Y, -p.X) }
func rot90CCW(p f32.Point) f32.Point { return f32.Pt(-p.Y, +p.X) }

func normPt(p f32.Point, l float32) f32.Point {
	d := math.Hypot(float64(p.X), float64(p.Y))
	l64 := float64(l)
	if math.Abs(d-l64) < 1e-10 {
		return f32.Point{}
	}
	n := float32(l64 / d)
	return f32.Point{X: p.X * n, Y: p.Y * n}
}

func dotPt(p, q f32.Point) float32 {
	return p.X*q.X + p.Y*q.Y
}

func perpDot(p, q f32.Point) float32 {
	return p.X*q.Y - p.Y*q.X
}

// strokePathCurv returns the curvature at t, along the quadratic Bézier
// curve defined by the triplet (beg, ctl, end).
func strokePathCurv(beg, ctl, end f32.Point, t float32) float32 {
	var (
		d1p = quadBezierD1(beg, ctl, end, t)
		d2p = quadBezierD2(beg, ctl, end, t)

		// Negative when bending right, ie: the curve is CW at this point.
		a = float64(perpDot(d1p, d2p))
	)

	// We check early that the segment isn't too line-like and
	// save a costly call to math.Pow that will be discarded by dividing
	// with a too small 'a'.
	if math.Abs(a) < 1e-10 {
		return float32(math.NaN())
	}
	return float32(math.Pow(float64(d1p.X*d1p.X+d1p.Y*d1p.Y), 1.5) / a)
}

// quadBezierSample returns the point on the Bézier curve at t.
//  B(t) = (1-t)^2 P0 + 2(1-t)t P1 + t^2 P2
func quadBezierSample(p0, p1, p2 f32.Point, t float32) f32.Point {
	t1 := 1 - t
	c0 := t1 * t1
	c1 := 2 * t1 * t
	c2 := t * t

	o := p0.Mul(c0)
	o = o.Add(p1.Mul(c1))
	o = o.Add(p2.Mul(c2))
	return o
}

// quadBezierD1 returns the first derivative of the Bézier curve with respect to t.
//  B'(t) = 2(1-t)(P1 - P0) + 2t(P2 - P1)
func quadBezierD1(p0, p1, p2 f32.Point, t float32) f32.Point {
	p10 := p1.Sub(p0).Mul(2 * (1 - t))
	p21 := p2.Sub(p1).Mul(2 * t)

	return p10.Add(p21)
}

// quadBezierD2 returns the second derivative of the Bézier curve with respect to t:
//  B''(t) = 2(P2 - 2P1 + P0)
func quadBezierD2(p0, p1, p2 f32.Point, t float32) f32.Point {
	p := p2.Sub(p1.Mul(2)).Add(p0)
	return p.Mul(2)
}

func strokeQuadBezier(state strokeState, d, flatness float32) strokeQuads {
	// Gio strokes are only quadratic Bézier curves, w/o any inflection point.
	// So we just have to flatten them.
	var qs strokeQuads
	return flattenQuadBezier(qs, state.p0, state.ctl, state.p1, d, flatness)
}

// flattenQuadBezier splits a Bézier quadratic curve into linear sub-segments,
// themselves also encoded as Bézier (degenerate, flat) quadratic curves.
func flattenQuadBezier(qs strokeQuads, p0, p1, p2 f32.Point, d, flatness float32) strokeQuads {
	var t float32
	for t < 1 {
		s2 := float64((p2.X-p0.X)*(p1.Y-p0.Y) - (p2.Y-p0.Y)*(p1.X-p0.X))
		den := math.Hypot(float64(p1.X-p0.X), float64(p1.Y-p0.Y))
		if s2*den == 0.0 {
			break
		}

		s2 /= den
		flat64 := float64(flatness)
		t = 2.0 * float32(math.Sqrt(flat64/3.0/math.Abs(s2)))
		if t >= 1.0 {
			break
		}
		var q0, q1, q2 f32.Point
		q0, q1, q2, p0, p1, p2 = quadBezierSplit(p0, p1, p2, t)
		qs.addLine(q0, q1, q2, 0, d)
	}
	qs.addLine(p0, p1, p2, 1, d)
	return qs
}

func (qs *strokeQuads) addLine(p0, ctrl, p1 f32.Point, t, d float32) {
	p0 = p0.Add(strokePathNorm(p0, ctrl, p1, 0, d))
	p1 = p1.Add(strokePathNorm(p0, ctrl, p1, 1, d))

	*qs = append(*qs,
		strokeQuad{
			quad: ops.Quad{
				From: p0,
				Ctrl: p0.Add(p1).Mul(0.5),
				To:   p1,
			},
		},
	)
}

// quadInterp returns the interpolated point at t.
func quadInterp(p, q f32.Point, t float32) f32.Point {
	return f32.Pt(
		(1-t)*p.X+t*q.X,
		(1-t)*p.Y+t*q.Y,
	)
}

// quadBezierSplit returns the pair of triplets (from,ctrl,to) Bézier curve,
// split before (resp. after) the provided parametric t value.
func quadBezierSplit(p0, p1, p2 f32.Point, t float32) (f32.Point, f32.Point, f32.Point, f32.Point, f32.Point, f32.Point) {

	var (
		b0 = p0
		b1 = quadInterp(p0, p1, t)
		b2 = quadBezierSample(p0, p1, p2, t)

		a0 = b2
		a1 = quadInterp(p1, p2, t)
		a2 = p2
	)

	return b0, b1, b2, a0, a1, a2
}

// strokePathJoin joins the two paths rhs and lhs, according to the provided
// stroke style sty.
func strokePathJoin(sty clip.StrokeStyle, rhs, lhs *strokeQuads, hw float32, pivot, n0, n1 f32.Point, r0, r1 float32) {
	strokePathBevelJoin(rhs, lhs, hw, pivot, n0, n1, r0, r1)
}

func strokePathBevelJoin(rhs, lhs *strokeQuads, hw float32, pivot, n0, n1 f32.Point, r0, r1 float32) {

	rp := pivot.Add(n1)
	lp := pivot.Sub(n1)

	rhs.lineTo(rp)
	lhs.lineTo(lp)
}

// strokePathCap caps the provided path qs, according to the provided stroke style sty.
func strokePathCap(sty clip.StrokeStyle, qs *strokeQuads, hw float32, pivot, n0 f32.Point) {
	switch sty.Cap {
	case clip.FlatCap:
		strokePathFlatCap(qs, hw, pivot, n0)
	case clip.SquareCap:
		strokePathSquareCap(qs, hw, pivot, n0)
	case clip.RoundCap:
		strokePathRoundCap(qs, hw, pivot, n0)
	default:
		panic("impossible")
	}
}

// strokePathFlatCap caps the start or end of a path with a flat cap.
func strokePathFlatCap(qs *strokeQuads, hw float32, pivot, n0 f32.Point) {
	end := pivot.Sub(n0)
	qs.lineTo(end)
}

// strokePathSquareCap caps the start or end of a path with a square cap.
func strokePathSquareCap(qs *strokeQuads, hw float32, pivot, n0 f32.Point) {
	var (
		e       = pivot.Add(rot90CCW(n0))
		corner1 = e.Add(n0)
		corner2 = e.Sub(n0)
		end     = pivot.Sub(n0)
	)

	qs.lineTo(corner1)
	qs.lineTo(corner2)
	qs.lineTo(end)
}

// strokePathRoundCap caps the start or end of a path with a round cap.
func strokePathRoundCap(qs *strokeQuads, hw float32, pivot, n0 f32.Point) {
	c := pivot.Sub(qs.pen())
	qs.arc(c, c, math.Pi)
}
