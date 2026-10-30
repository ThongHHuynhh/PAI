package tui

import (
	"math"
	"time"

	"pai/pkg/connector/tui/term"
)

// Orbit is the home screen's animation: the PAI core, breathing, with the
// machines it connects on tilted orbits around it. Each machine has a link to
// the core, and a pulse runs along each link, as data does. The scene tilts
// toward the mouse; a key pulls every node into the core, which then closes.
// It draws in braille, 2x4 dots a cell, so a dot is square.
const (
	orbitFrame    = time.Second / 30
	orbitNodes    = 6    // nodes drawn when the screen knows of no machines
	orbitMaxNodes = 12   // more machines than this would crowd the rings
	orbitPitch    = 18.0 // degrees: the base tilt toward the viewer
	orbitTilt     = 22.0 // degrees toward the mouse at orbitReach
	orbitReach    = 1.5  // the pointer's distance from the center, in half screens, that gives the full tilt
	orbitEase     = 0.45 // seconds the pose takes to follow the mouse
	orbitFillW    = 0.42 // share of the width the outer ring spans
	orbitFillH    = 0.9  // share of the height
	orbitCore     = 0.2  // the core's radius as a share of the outer ring's
	orbitBreathe  = 0.6  // breaths a second
	orbitWave     = 0.45 // waves a second leaving the core
	orbitPulse    = 0.5  // link crossings a second
	orbitStrength = 0.85 // the inks, from the screen's color toward the text's and the accent's
	orbitDim      = 0.45 // the rings' ink, fainter than the nodes'
	collapseDur   = 1.3  // seconds from the key until the core is closed
	collapseAfter = 0.2  // seconds from closed to the next screen
)

const rad = math.Pi / 180

// rings are the orbits: radius as a share of the outer ring's, inclination
// in degrees, node of the inclination in degrees, and turns a second.
var rings = [3]struct{ r, inc, node, speed float64 }{
	{0.55, 12, 0, 0.07},
	{0.78, -24, 50, -0.05},
	{1.0, 38, -35, 0.035},
}

type orbit struct {
	on       func() bool
	nodes    func() int // machines connected now; none or 0 draws orbitNodes
	t        float64    // seconds of animation so far, which stops while the terminal is blurred
	last     time.Time
	flying   bool    // a key started the collapse
	fly      float64 // t at that key
	ex, ey   float64 // the eased mouse
	px       []uint8 // per dot: 0 none, 1 dim, 2 text ink, 3 accent ink
	pw, ph   int
	cx, cy   float64
	scale    float64
	collapse float64 // 0 to 1
}

// flown is whether the collapse and the pause after it are over.
func (o *orbit) flown() bool {
	return o.flying && o.t-o.fly >= collapseDur+collapseAfter
}

type v3 [3]float64

// rotX turns p about the x axis by a, rotY about y.
func rotX(p v3, a float64) v3 {
	s, c := math.Sincos(a)
	return v3{p[0], p[1]*c - p[2]*s, p[1]*s + p[2]*c}
}

func rotY(p v3, a float64) v3 {
	s, c := math.Sincos(a)
	return v3{p[0]*c + p[2]*s, p[1], -p[0]*s + p[2]*c}
}

// view is p in the viewer's frame: the scene's yaw and pitch from the mouse.
func (o *orbit) view(p v3) v3 {
	return rotX(rotY(p, -o.ex*orbitTilt*rad), (orbitPitch+o.ey*orbitTilt*.6)*rad)
}

// onRing is the point at angle th on ring i, before the view.
func onRing(i int, th float64) v3 {
	g := rings[i]
	s, c := math.Sincos(th)
	return rotY(rotX(v3{g.r * c, 0, g.r * s}, g.inc*rad), g.node*rad)
}

func (o *orbit) dot(x, y float64, ink uint8) {
	ix, iy := int(math.Round(x)), int(math.Round(y))
	if ix < 0 || iy < 0 || ix >= o.pw || iy >= o.ph {
		return
	}
	if p := &o.px[iy*o.pw+ix]; ink > *p {
		*p = ink
	}
}

// project is p's dot position and depth, z toward the viewer.
func (o *orbit) project(p v3) (x, y, z float64) {
	return o.cx + p[0]*o.scale, o.cy - p[1]*o.scale, p[2]
}

// hidden is whether a point at x, y and depth z is behind the core.
func (o *orbit) hidden(x, y, z, core float64) bool {
	dx, dy := x-o.cx, y-o.cy
	return z < 0 && dx*dx+dy*dy < core*core
}

func (o *orbit) disc(x, y, r float64, ink uint8) {
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			if dx*dx+dy*dy <= r*r+.5 {
				o.dot(x+dx, y+dy, ink)
			}
		}
	}
}

func (o *orbit) Draw(a *App, c *Canvas, r Rect) {
	now := time.Now()
	if o.last.IsZero() {
		o.last = now
	}
	dt := min(0.1, now.Sub(o.last).Seconds())
	o.last = now
	if a.Blurred {
		dt = 0
	}
	o.t += dt
	if !o.on() || r.H <= 0 || r.W <= 0 {
		return
	}
	if o.flying {
		u := (o.t - o.fly) / collapseDur
		if u >= 1 {
			return
		}
		o.collapse = smooth(u)
	}
	var mx, my float64
	if a.MouseX >= 0 {
		mx = clamp1((float64(a.MouseX) + .5 - float64(r.W)/2) / (float64(r.W) / 2) / orbitReach)
		my = clamp1((float64(a.MouseY) + .5 - float64(r.H)/2) / (float64(r.H) / 2) / orbitReach)
	}
	k := 1 - math.Exp(-dt/orbitEase)
	o.ex += (mx - o.ex) * k
	o.ey += (my - o.ey) * k

	o.pw, o.ph = r.W*2, r.H*4
	if n := o.pw * o.ph; cap(o.px) < n {
		o.px = make([]uint8, n)
	} else {
		o.px = o.px[:n]
		clear(o.px)
	}
	o.cx, o.cy = float64(o.pw)/2, float64(o.ph)/2
	o.scale = min(float64(o.pw)*orbitFillW, float64(o.ph)*orbitFillH/2)
	o.render()

	inks := [4]term.Color{0, a.Theme.BG.Blend(a.Theme.Text, orbitStrength*orbitDim), a.Theme.BG.Blend(a.Theme.Text, orbitStrength), a.Theme.BG.Blend(a.Theme.Accent, orbitStrength)}
	for y := range r.H {
		// text, and a few cells either side of it, stay clear
		text := make([]bool, r.W)
		last := -9
		for x := range r.W {
			if cl := c.b.At(r.X+x, r.Y+y); cl.G != " " || cl.BG != a.Theme.BG {
				for i := max(0, last+1); x-last <= 4 && i <= x; i++ {
					text[i] = true
				}
				text[x], last = true, x
			}
		}
		for x := range r.W {
			if text[x] {
				continue
			}
			var bits, ink uint8
			for dy := range 4 {
				for dx := range 2 {
					if p := o.px[(y*4+dy)*o.pw+x*2+dx]; p != 0 {
						bits |= brailleBit[dy][dx]
						ink = max(ink, p)
					}
				}
			}
			if bits != 0 {
				c.Set(r.X+x, r.Y+y, string(rune(0x2800)+rune(bits)), 1, term.Style{FG: inks[ink]})
			}
		}
	}
}

// brailleBit is the bit of each dot of a braille cell, by row and column.
var brailleBit = [4][2]uint8{{0x01, 0x08}, {0x02, 0x10}, {0x04, 0x20}, {0x40, 0x80}}

func (o *orbit) render() {
	pull := 1 - o.collapse // every node and ring falls toward the core
	breathe := 1 + 0.08*math.Sin(o.t*orbitBreathe*2*math.Pi)
	core := orbitCore * o.scale * breathe * (1 - o.collapse*o.collapse)
	// rings: a dotted path, every third step, fainter than the nodes
	for i := range rings {
		steps := int(2 * math.Pi * rings[i].r * o.scale)
		for s := 0; s < steps; s += 3 {
			p := onRing(i, 2*math.Pi*float64(s)/float64(steps))
			x, y, z := o.project(o.view(v3{p[0] * pull, p[1] * pull, p[2] * pull}))
			if !o.hidden(x, y, z, core) {
				o.dot(x, y, 1)
			}
		}
	}
	// nodes, their links, and a pulse along each link
	n := orbitNodes
	if o.nodes != nil {
		if m := o.nodes(); m > 0 {
			n = min(m, orbitMaxNodes)
		}
	}
	for j := range n {
		i := j % len(rings)
		th := 2*math.Pi*(rings[i].speed*o.t+float64(j)/float64(n)) + float64(i)
		p := onRing(i, th)
		x, y, z := o.project(o.view(v3{p[0] * pull, p[1] * pull, p[2] * pull}))
		if o.hidden(x, y, z, core) {
			continue
		}
		dx, dy := x-o.cx, y-o.cy
		d := math.Hypot(dx, dy)
		if d > core+2 {
			ux, uy := dx/d, dy/d
			for s := core + 1; s < d-3; s += 2 {
				o.dot(o.cx+ux*s, o.cy+uy*s, 1)
			}
			f := math.Mod(o.t*orbitPulse+float64(j)*0.37, 1)
			if j%2 == 1 { // half the links carry data in, half out
				f = 1 - f
			}
			s := core + 1 + f*(d-core-4)
			o.disc(o.cx+ux*s, o.cy+uy*s, 1, 3)
		}
		size := 1.5 + 0.8*(z+1) // nearer is larger
		o.disc(x, y, size*pull+0.5, 2)
	}
	// a wave leaves the core and fades as it spreads
	if o.collapse == 0 {
		w := math.Mod(o.t*orbitWave, 1)
		wr := core * (1 + 2.2*w)
		steps := int(2 * math.Pi * wr)
		every := 1 + int(w*6)
		for s := 0; s < steps; s += every {
			th := 2 * math.Pi * float64(s) / float64(steps)
			o.dot(o.cx+wr*math.Cos(th), o.cy+wr*math.Sin(th), 1)
		}
	}
	// the core: a lit sphere, its dots thinning away from the light
	for y := -core; y <= core; y++ {
		for x := -core; x <= core; x++ {
			q := x*x + y*y
			if q > core*core {
				continue
			}
			nz := math.Sqrt(1 - q/(core*core))
			light := clamp01(0.25 + 0.75*(nz*0.8+(-x/core)*0.35+(-y/core)*0.35))
			ix, iy := int(o.cx+x), int(o.cy+y)
			if light > bayer[(iy&3)*4+ix&3]/16 {
				o.dot(o.cx+x, o.cy+y, 3)
			}
		}
	}
}

// bayer is a 4x4 ordered dither: a dot is on where its light passes.
var bayer = [16]float64{0, 8, 2, 10, 12, 4, 14, 6, 3, 11, 1, 9, 15, 7, 13, 5}

func clamp01(v float64) float64 { return max(0, min(1, v)) }
func clamp1(v float64) float64  { return max(-1, min(1, v)) }
func smooth(u float64) float64  { u = clamp01(u); return u * u * (3 - 2*u) }
