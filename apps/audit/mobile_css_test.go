package audit

import (
	"regexp"
	"testing"
)

// The home page once scrolled sideways on a phone -- 695px of content at a 390px
// screen, with the code panels clipped -- and Google judges the mobile page.
// Layout cannot be measured in a unit test, so these pin the four rules the fix
// rests on. Each one was measured in a real browser at 360 and 390px to be what
// stopped a specific element from widening the document; the browser check is in
// the pull request that added them.
func TestStylesheetKeepsPhoneLayoutFromWideningThePage(t *testing.T) {
	css := string(appCSS)
	rules := map[string]string{
		// A single-column grid track is auto-sized, so a wide <pre> inside it set
		// the track to its own width. minmax(0, 1fr) lets the pre scroll in its box.
		"the stacked step cards use a shrinkable track": `(?s)@media \(max-width: 900px\) \{[^}]*\.steps \{ grid-template-columns: minmax\(0, 1fr\); \}`,
		// Same cause inside the tabbed code panes: the one grid cell they share.
		"the tabbed code panes share a shrinkable track": `\.stack\[data-tabbed\] \{ display: grid; grid-template-columns: minmax\(0, 1fr\); \}`,
		// An unbroken URL in <code> has nowhere to wrap.
		"inline code may break inside a long address": `(?m)^code \{ overflow-wrap: anywhere; \}`,
		// The visually hidden .sr spans inside the table are absolutely positioned
		// and sat at their static position, far right, unless something contains them.
		"the table's scroll box contains its hidden spans": `(?s)\.board \.scroll \{[^}]*position: relative;`,
	}
	for name, pattern := range rules {
		if !regexp.MustCompile(pattern).MatchString(css) {
			t.Errorf("%s: rule missing from app.css", name)
		}
	}
}
