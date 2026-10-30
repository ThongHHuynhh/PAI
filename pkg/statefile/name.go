package statefile

import (
	"fmt"
	"math/rand/v2"
	"regexp"
)

// nameTries is how many two-word names NewName draws before it adds a
// number: with 14,400 pairs, a person's sessions rarely fill a draw.
const nameTries = 20

// A session id is two everyday words, such as calm-otter: short to say and to
// type, and plain for another agent to pass along. The words are common,
// easy to spell, and never form anything rude together.
var (
	adjectives = []string{
		"amber", "azure", "bold", "brave", "bright", "brisk", "calm", "clear", "clever", "cool",
		"cosy", "crisp", "curly", "daring", "dawn", "deep", "eager", "early", "easy", "fair",
		"fancy", "fast", "fine", "firm", "fluffy", "fresh", "friendly", "gentle", "giant", "glad",
		"golden", "grand", "green", "happy", "hardy", "hazel", "honest", "humble", "icy", "jolly",
		"keen", "kind", "lively", "lucky", "lunar", "magic", "mellow", "merry", "mighty", "minty",
		"misty", "modest", "neat", "nimble", "noble", "olive", "orange", "pale", "patient", "peach",
		"plain", "polite", "proud", "purple", "quick", "quiet", "rapid", "ready", "rosy", "royal",
		"ruby", "rusty", "sandy", "sharp", "shiny", "silent", "silver", "simple", "sleek", "smart",
		"snowy", "solar", "solid", "sonic", "spicy", "steady", "still", "stormy", "sturdy", "sunny",
		"super", "sweet", "swift", "tidy", "tiny", "true", "vivid", "warm", "wild", "witty",
		"breezy", "cheery", "dusty", "frosty", "grassy", "hidden", "jade", "lemon", "mossy", "pearl",
		"rainy", "rocky", "scarlet", "spring", "summer", "tawny", "velvet", "windy", "winter", "zesty",
	}
	nouns = []string{
		"otter", "falcon", "maple", "river", "comet", "badger", "beacon", "birch", "bison", "breeze",
		"brook", "cactus", "canyon", "cedar", "cloud", "clover", "coral", "crane", "cricket", "dolphin",
		"dove", "eagle", "ember", "fern", "finch", "fjord", "forest", "fox", "garden", "gecko",
		"glacier", "harbor", "hawk", "heron", "hill", "island", "ivy", "jaguar", "koala", "lagoon",
		"lake", "lark", "lemur", "lily", "lion", "lotus", "lynx", "meadow", "meteor", "moose",
		"moon", "moss", "newt", "oak", "ocean", "orbit", "orchid", "owl", "panda", "parrot",
		"pebble", "pelican", "penguin", "pine", "planet", "pond", "poppy", "prairie", "puffin", "quail",
		"rabbit", "raven", "reef", "robin", "rocket", "sage", "salmon", "seal", "shore", "sparrow",
		"spruce", "squirrel", "star", "stone", "stream", "summit", "swan", "thistle", "thunder", "tiger",
		"tulip", "turtle", "valley", "violet", "walrus", "willow", "wolf", "wren", "yak", "zebra",
		"acorn", "aspen", "bamboo", "bay", "cliff", "delta", "dune", "fig", "grove", "hazel",
		"kiwi", "lantern", "mango", "mesa", "nectar", "pecan", "quartz", "ridge", "tundra", "wave",
	}
	nameID = regexp.MustCompile(`^[a-z]{2,10}-[a-z]{2,10}(-[0-9]{1,4})?$`)
)

// NewName is a new session id that taken does not hold: two words, or two
// words and a number once nameTries draws were all in use.
func NewName(taken func(id string) bool) string {
	pick := func() string {
		return adjectives[rand.IntN(len(adjectives))] + "-" + nouns[rand.IntN(len(nouns))]
	}
	for range nameTries {
		if id := pick(); !taken(id) {
			return id
		}
	}
	for {
		if id := fmt.Sprintf("%s-%d", pick(), 2+rand.IntN(9998)); !taken(id) {
			return id
		}
	}
}
