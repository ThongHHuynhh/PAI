# Renders logo.html frame by frame into docs/assets/pai.gif:
#   uv run --no-project --with playwright --with pillow python docs/assets/logo/render.py docs/assets/logo/logo.html docs/assets/pai.gif
import io, sys, pathlib
from playwright.sync_api import sync_playwright
from PIL import Image

page_path, out = pathlib.Path(sys.argv[1]).resolve(), sys.argv[2]
LOOP, FPS = 8, 20  # 50 ms a frame: GIF delays are in hundredths of a second
frames = []
with sync_playwright() as p:
    b = p.chromium.launch(channel="msedge")
    pg = b.new_page(viewport={"width": 720, "height": 240}, device_scale_factor=2)
    pg.goto(page_path.as_uri())
    for k in range(LOOP * FPS):
        pg.evaluate(f"draw({k / FPS})")
        png = pg.locator(".stage").screenshot()
        frames.append(Image.open(io.BytesIO(png)).convert("RGB").resize((720, 240), Image.LANCZOS))
    b.close()
# one palette for every frame, so what stays still keeps its exact color;
# undithered, since dither noise would shimmer frame to frame
# Median cut averages near colors, and Pillow's lookup is approximate, so
# entries within a few shades of the page white or the wordmark ink become
# exactly that: the background stays #ffffff, the letters #17151f.
pal = frames[0].quantize(colors=256, method=Image.Quantize.MEDIANCUT)
EXACT = [(0xff, 0xff, 0xff), (0x17, 0x15, 0x1f)]
rgb = pal.getpalette()
for i in range(0, len(rgb), 3):
    for c in EXACT:
        if max(abs(a - b) for a, b in zip(rgb[i:i + 3], c)) <= 12:
            rgb[i:i + 3] = c
pal.putpalette(rgb)
q = [f.quantize(palette=pal, dither=Image.Dither.NONE) for f in frames]
q[0].save(out, save_all=True, append_images=q[1:], duration=round(1000 / FPS), loop=0, optimize=True, disposal=1)
print(len(q), "frames")
