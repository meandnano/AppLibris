# Two button styles for one button

`internal/web/static/css/app.css` now defines two near-identical button
systems: `.send__button` (with `--primary`/`--secondary`), added with the
send control, and the generic `.button` (same modifiers), added with the
inline metadata editors. They share font, size, letter-spacing, uppercase
transform, inline-flex layout and a 9px gap; they differ in padding, and
in `--primary`'s foreground (`#fff` versus `var(--bg-raised)`).

Nothing is broken — both render correctly in light and dark — so this is
not a plan. It is left here because the next control that needs a button
has to pick one arbitrarily, and the pair will keep drifting until they
are folded together.

The reason it wasn't done in the editing change: collapsing them means
restyling the send control, which had just shipped after manual browser
verification across its five states. Deduplicating CSS is not worth
risking a regression in a feature that was verified by eye rather than by
test.

A fix would promote `.button` to the shared base, leave `.send__button`
holding only its larger padding, and reconcile the two `--primary`
foregrounds — checking the send control's four states in both themes
afterwards, since that is what the change puts at risk.
