# UI font

`ui.woff2` is **Space Grotesk**, a variable font (weight 300-700) under the SIL
Open Font License 1.1. The licence is in `OFL.txt` and both files are committed,
because OFL fonts may be redistributed. It is subset to Latin plus the few
symbols the dashboard draws, which is why it is ~31 KB rather than ~137 KB.

Chosen because this dashboard is mostly monospace and Space Grotesk was drawn as
the proportional companion to Space Mono, so the two sit together by design.

## Using a different font

Drop any variable `.woff2` here as `ui.woff2`. It is read from disk at request
time, so a swap needs no rebuild and no restart, just a hard reload:

    cp ~/Downloads/YourFont.woff2 fonts/ui.woff2

If you hold a licence for a font you may NOT redistribute, keep it out of the
repository and point `FONT_PATH` somewhere else instead.

## No font at all

Delete `ui.woff2` and `/fonts/ui.woff2` returns 404, the browser falls through
the `--sans` stack, and the dashboard renders in the system UI font. A missing
font is never an error.

Served from the binary rather than a CDN on purpose: a webfont request is still
a request that leaves the machine and reports when someone opened a tool pointed
at their own notes.

## Rebuilding the subset

    curl -sLO https://github.com/google/fonts/raw/main/ofl/spacegrotesk/SpaceGrotesk%5Bwght%5D.ttf
    python3 -m fontTools.subset "SpaceGrotesk[wght].ttf" \
      --unicodes="U+0000-00FF,U+0100-017F,U+2000-206F,U+20AC,U+2122,U+2190-2193,U+2212,U+25B2,U+25B8,U+25BC,U+25BE,U+2713,U+2717,U+2197,U+00B7" \
      --layout-features='*' --flavor=woff2 --output-file=ui.woff2
