# Renders logo.html frame by frame into docs/assets/pai.gif:
#   uv run --no-project --with playwright --with pillow python docs/assets/logo/render.py docs/assets/logo/logo.html docs/assets/pai.gif
import io, sys, pathlib
from playwright.sync_api import sync_playwright
from PIL import Image

page_path, out = pathlib.Path(sys.argv[1]).resolve(), sys.argv[2]
LOOP, FPS = 8, 15
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
# a palette per frame, undithered: the theme fade needs in-between colors that
# one shared palette lacks, and dither noise would shimmer frame to frame
q = [f.quantize(colors=256, method=Image.Quantize.MEDIANCUT, dither=Image.Dither.NONE) for f in frames]
q[0].save(out, save_all=True, append_images=q[1:], duration=round(1000 / FPS), loop=0, optimize=True, disposal=1)
print(len(q), "frames")
