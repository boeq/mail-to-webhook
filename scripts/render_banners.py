#!/usr/bin/env python3
"""Render the README's header and footer images from .github/assets/banner.html.

Needs a Chromium. It uses $CHROME when set, then one on the PATH, then the one
Playwright downloads (`playwright install chromium`). The fonts come from the
network, so this needs a connection.
"""
import glob
import os
import pathlib
import shutil
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
ASSETS = ROOT / ".github" / "assets"
SIZES = {"header": (1280, 320), "footer": (1280, 104)}


def chromium():
    if os.environ.get("CHROME"):
        return os.environ["CHROME"]
    for name in ("chromium", "chromium-browser", "google-chrome", "google-chrome-stable"):
        if shutil.which(name):
            return shutil.which(name)
    cache = pathlib.Path.home() / ".cache" / "ms-playwright"
    for pattern in ("chromium_headless_shell-*/chrome-*/chrome-headless-shell", "chromium-*/chrome-linux*/chrome"):
        found = sorted(glob.glob(str(cache / pattern)))
        if found:
            return found[-1]
    sys.exit("no Chromium found: set CHROME, or run `playwright install chromium`")


def main():
    browser = chromium()
    source = (ASSETS / "banner.html").as_uri()
    for kind, (width, height) in SIZES.items():
        for theme in ("light", "dark"):
            target = ASSETS / f"{kind}-{theme}.png"
            subprocess.run([
                browser, "--headless", "--no-sandbox", "--hide-scrollbars",
                "--default-background-color=00000000",
                "--force-device-scale-factor=2",
                f"--window-size={width},{height}",
                "--virtual-time-budget=8000",
                f"--screenshot={target}",
                f"{source}?kind={kind}&theme={theme}",
            ], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            print(f"wrote {target.relative_to(ROOT)}")


if __name__ == "__main__":
    main()
