package views

import (
	"math/rand/v2"
	"sync"

	"github.com/charmbracelet/bubbles/spinner"
)

// spinnerPhrases is the pool of "thinking" labels the spinner picks
// from each time work kicks off. Mix of pure-silly, tech humor, and
// pop-culture references — chosen once per turn (not per frame) so
// the user reads one full phrase rather than a flicker of nonsense.
//
// Bar for inclusion: makes the developer smile without making them
// look up a reference. Excludes anything that punches down or
// that's likely to read as inappropriate over a colleague's
// shoulder. If a phrase makes you wince, replace it.
var spinnerPhrases = []string{
	// Pure-silly verbs
	"flibbertigibbeting…",
	"discombobulating…",
	"wibbling and wobbling…",
	"swizzling…",
	"noodling…",
	"ruminating…",
	"marinating ideas…",
	"polishing apostrophes…",
	"fermenting thoughts…",
	"decanting logic…",
	"embiggening the perfectly cromulent…",
	"untangling the slinky…",
	"kneading the dough…",
	"snorkeling through stack frames…",
	"yodeling at the abyss…",
	"bamboozling the bits…",
	"tickling the keyboard…",
	"polishing the abacus…",
	"spinning the prayer wheel…",
	"adjusting the chakras…",
	"shooshing the gremlins…",
	"refactoring vibes…",
	"thinking really hard…",
	"squinting at the problem…",
	"counting on fingers…",
	"chewing on it…",
	"sleeping on it (briefly)…",
	"sniffing around…",
	"rummaging in the drawer…",
	"warming up the brain…",

	// Tech humor
	"interrogating the rubber duck…",
	"blaming the cache…",
	"yak shaving…",
	"bikeshedding…",
	"considering the off-by-one…",
	"googling stack overflow…",
	"compiling justifications…",
	"linting the soul…",
	"deferring panic…",
	"wrangling pointers…",
	"negotiating with the linker…",
	"vendoring our dreams…",
	"git-blaming the past…",
	"resolving merge conflicts in spirit…",
	"writing the TODO…",
	"renaming variables…",
	"adding a semicolon…",
	"closing the parenthesis…",
	"buffering like it's 1999…",
	"defragmenting…",
	"phoning the help desk…",
	"reading the manual (finally)…",
	"asking the senior dev…",
	"rebooting the universe…",
	"clearing the ca$h…",
	"escalating to the cloud…",
	"summoning the daemons…",
	"chmod +x'ing the brain…",

	// Pop culture
	"summoning a patronus…",
	"rolling for initiative…",
	"consulting the oracle…",
	"spinning up the flux capacitor…",
	"hacking the gibson…",
	"calibrating the deflector array…",
	"computing the answer to 42…",
	"channeling the bene gesserit…",
	"warming up the TARDIS…",
	"negotiating with the kraken…",
	"polishing the one ring…",
	"asking the magic 8-ball…",
	"consulting jarvis…",
	"rolling natural twenties…",
	"loading the konami code…",
	"feeding the tamagotchi…",
	"running the hyperdrive checklist…",
	"decrypting the enigma…",
	"petting schrödinger's cat…",
	"making it so…",
	"engaging the warp drive…",
	"befriending HAL…",
	"calibrating the holodeck…",
	"asking GLaDOS nicely…",
	"speaking parseltongue…",
	"pulling the lever, kronk…",
	"summoning the genie…",
	"rewinding the VHS…",
	"polishing the tesseract…",
	"watering the ents…",
	"convincing the bouncer…",
	"feeding the mogwai (before midnight)…",
	"asking the dude to abide…",
	"yelling at clouds…",
	"poking the panopticon…",
	"reticulating splines…",
	"finishing the chocolate frog…",
	"checking under the desk for goombas…",
	"reading the EULA (jk)…",
	"opening the pickle jar…",
	"rubbing the lamp…",
	"shouting into the void…",
	"feeding the office plant…",
}

var spinnerRng = struct {
	mu sync.Mutex
}{}

// pickSpinnerPhrase returns a random phrase from the pool. Uses the
// global rand source (math/rand/v2 is safe for concurrent use); the
// mutex is just to guarantee a different phrase from the
// most-recently-emitted one so consecutive turns don't repeat.
var lastSpinnerPhrase string

func pickSpinnerPhrase() string {
	spinnerRng.mu.Lock()
	defer spinnerRng.mu.Unlock()
	for i := 0; i < 4; i++ {
		p := spinnerPhrases[rand.IntN(len(spinnerPhrases))]
		if p != lastSpinnerPhrase {
			lastSpinnerPhrase = p
			return p
		}
	}
	return spinnerPhrases[rand.IntN(len(spinnerPhrases))]
}

// spinnerStyles is the curated pool of bubbles spinner animations the
// TUI rotates through per turn. Excludes Monkey/Hamburger/Meter — they
// either read as "loading bar finished" (Meter) or are too tall/wide
// for an inline status line. Excludes Ellipsis — its frames have
// variable width (""/"."/".."/"...") so the phrase to its right
// shifts each frame, which reads as instability.
var spinnerStyles = []spinner.Spinner{
	spinner.MiniDot,
	spinner.Dot,
	spinner.Line,
	spinner.Jump,
	spinner.Pulse,
	spinner.Points,
	spinner.Globe,
	spinner.Moon,
}

var lastSpinnerStyleIdx = -1

// pickSpinnerStyle returns a spinner animation different from the
// previously-returned one so consecutive turns visibly differ.
func pickSpinnerStyle() spinner.Spinner {
	spinnerRng.mu.Lock()
	defer spinnerRng.mu.Unlock()
	idx := rand.IntN(len(spinnerStyles))
	for i := 0; i < 4 && idx == lastSpinnerStyleIdx; i++ {
		idx = rand.IntN(len(spinnerStyles))
	}
	lastSpinnerStyleIdx = idx
	return spinnerStyles[idx]
}
