package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"
)

// Arms are how many items a recall brings: 0 skips recall for the turn.
var Arms = []int{0, 3, 6, 10}

// alpha is LinUCB's exploration: how far an arm's upper bound reaches past
// its estimate while it is little tried.
const alpha = 0.6

// dims is the width of a turn's features; see features.
const dims = 6

// features describe a turn to the bandit: a bias, whether the text is short,
// whether it asks, its length, how full the context is, and how much there is
// to recall.
func features(t Turn, items int) []float64 {
	q := strings.TrimSpace(t.Query)
	short, asks := 0.0, 0.0
	if len(q) < 40 {
		short = 1
	}
	if strings.Contains(q, "?") || startsWithAny(strings.ToLower(q), "what", "how", "why", "when", "where", "who", "which", "do ", "does", "did", "can", "remember", "recall") {
		asks = 1
	}
	return []float64{1, short, asks, math.Log1p(float64(len(Words(q)))) / 5, min(1, max(0, t.Fill)), math.Log1p(float64(items)) / 8}
}

func startsWithAny(s string, ps ...string) bool {
	for _, p := range ps {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// arm is one arm's ridge regression: A = I + sum x x', b = sum r x.
type arm struct {
	A      [dims * dims]float64
	B      [dims]float64
	N      int
	Reward float64 // sum of rewards, for the mean the knowledge view shows
}

func newArm() arm {
	var a arm
	for i := range dims {
		a.A[i*dims+i] = 1
	}
	return a
}

// ucb is the arm's estimate for x plus its exploration bonus.
func (a arm) ucb(x []float64) float64 {
	inv, ok := invert(a.A)
	if !ok {
		return 0
	}
	var theta, ax [dims]float64
	for i := range dims {
		for j := range dims {
			theta[i] += inv[i*dims+j] * a.B[j]
			ax[i] += inv[i*dims+j] * x[j]
		}
	}
	est, v := 0.0, 0.0
	for i := range dims {
		est += theta[i] * x[i]
		v += x[i] * ax[i]
	}
	return est + alpha*math.Sqrt(max(0, v))
}

func (s *Store) arms(ctx context.Context) (map[int]arm, error) {
	out := map[int]arm{}
	for _, k := range Arms {
		out[k] = newArm()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT arm, a, b, n, reward FROM bandit`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k, n int
		var as, bs string
		var r float64
		if err := rows.Scan(&k, &as, &bs, &n, &r); err != nil {
			return nil, err
		}
		a := newArm()
		if json.Unmarshal([]byte(as), &a.A) != nil || json.Unmarshal([]byte(bs), &a.B) != nil {
			continue // a row from another width starts over
		}
		a.N, a.Reward = n, r
		out[k] = a
	}
	return out, rows.Err()
}

// choose is the arm with the highest upper bound for x. A tie, as before any
// learning, goes to the middle arm, so recall starts on.
func (s *Store) choose(ctx context.Context, x []float64) (int, error) {
	as, err := s.arms(ctx)
	if err != nil {
		return 0, err
	}
	bk := Arms[len(Arms)/2]
	best := as[bk].ucb(x)
	for _, k := range Arms {
		if u := as[k].ucb(x); u > best+1e-9 {
			best, bk = u, k
		}
	}
	return bk, nil
}

// update adds a turn's reward to its arm.
func (s *Store) update(ctx context.Context, k int, x []float64, reward float64) error {
	if len(x) != dims {
		return errors.New("memory: features of another width")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	as, err := s.arms(ctx)
	if err != nil {
		return err
	}
	a, ok := as[k]
	if !ok {
		return nil // an arm no longer offered
	}
	for i := range dims {
		for j := range dims {
			a.A[i*dims+j] += x[i] * x[j]
		}
		a.B[i] += reward * x[i]
	}
	a.N++
	a.Reward += reward
	ab, _ := json.Marshal(a.A)
	bb, _ := json.Marshal(a.B)
	_, err = s.db.ExecContext(ctx, `INSERT INTO bandit (arm,a,b,n,reward) VALUES (?,?,?,?,?) ON CONFLICT(arm) DO UPDATE SET a=excluded.a, b=excluded.b, n=excluded.n, reward=excluded.reward`, k, string(ab), string(bb), a.N, a.Reward)
	return err
}

// ArmStats is one arm as the knowledge view shows it.
type ArmStats struct {
	K      int     `json:"k"`
	Pulls  int     `json:"pulls"`
	Reward float64 `json:"reward"` // mean
}

// Bandit is every arm's pulls and mean reward.
func (s *Store) Bandit(ctx context.Context) ([]ArmStats, error) {
	as, err := s.arms(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var out []ArmStats
	for _, k := range Arms {
		a := as[k]
		st := ArmStats{K: k, Pulls: a.N}
		if a.N > 0 {
			st.Reward = a.Reward / float64(a.N)
		}
		out = append(out, st)
	}
	return out, nil
}

// invert is m's inverse by Gauss-Jordan; false when m is singular, which A,
// I plus outer products, never is.
func invert(m [dims * dims]float64) ([dims * dims]float64, bool) {
	var inv [dims * dims]float64
	for i := range dims {
		inv[i*dims+i] = 1
	}
	for c := range dims {
		p := c
		for r := c + 1; r < dims; r++ {
			if math.Abs(m[r*dims+c]) > math.Abs(m[p*dims+c]) {
				p = r
			}
		}
		if math.Abs(m[p*dims+c]) < 1e-12 {
			return inv, false
		}
		for j := range dims {
			m[c*dims+j], m[p*dims+j] = m[p*dims+j], m[c*dims+j]
			inv[c*dims+j], inv[p*dims+j] = inv[p*dims+j], inv[c*dims+j]
		}
		d := m[c*dims+c]
		for j := range dims {
			m[c*dims+j] /= d
			inv[c*dims+j] /= d
		}
		for r := range dims {
			if r == c {
				continue
			}
			f := m[r*dims+c]
			for j := range dims {
				m[r*dims+j] -= f * m[c*dims+j]
				inv[r*dims+j] -= f * inv[c*dims+j]
			}
		}
	}
	return inv, true
}
