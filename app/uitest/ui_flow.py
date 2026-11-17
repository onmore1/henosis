#!/usr/bin/env python3
"""Drives two Henosis windows like two people on two computers.

Usage: ui_flow.py PATH_TO_HENOSIS_AGENT [PATH_TO_A_FLP]
Needs Python with playwright and an installed Chrome. Prints one line per
check and exits non-zero if any of them failed.
"""
import os, shutil, subprocess, sys, tempfile, time
from playwright.sync_api import sync_playwright

agent = os.path.abspath(sys.argv[1])
flp = os.path.abspath(sys.argv[2]) if len(sys.argv) > 2 else None
root = tempfile.mkdtemp(prefix="henosis-ui-")
failures = []
apps = []


def check(what, ok):
    print(("ok    " if ok else "FAIL  ") + what)
    if not ok:
        failures.append(what)


def wait(what, condition, seconds=25):
    """Polls from outside the page (its security policy forbids evaluated strings)."""
    deadline = time.time() + seconds
    while time.time() < deadline:
        if condition():
            return
        time.sleep(0.2)
    failures.append(what)
    print("FAIL  timed out waiting for: " + what)


def start_app(name, port):
    home = os.path.join(root, name)
    os.makedirs(home, exist_ok=True)
    env = dict(os.environ, HENOSIS_CONFIG=os.path.join(home, "cfg", "config.json"), HOME=home, USERPROFILE=home)
    apps.append(subprocess.Popen([agent, "--background", "--port", str(port)], env=env))
    return home


def slow_type(page, selector, text, who=""):
    """Type with pauses longer than the page's 2 second refresh."""
    page.click(selector)
    time.sleep(2.5)                       # focus must survive a refresh before the first key
    check(f"{who}{selector} still has the cursor after a refresh", page.evaluate("document.activeElement.id") == selector[1:])
    half = len(text) // 2
    page.keyboard.type(text[:half], delay=50)
    time.sleep(2.5)                       # ... and in the middle of typing
    page.keyboard.type(text[half:], delay=50)
    time.sleep(2.5)                       # ... and after it
    check(f"{who}{selector} kept what was typed", page.input_value(selector) == text)


def file_is(path, content):
    return lambda: os.path.exists(path) and open(path, "rb").read() == content


ANN_PORT, BOB_PORT = 47833, 47834
ann_home = start_app("ann", ANN_PORT)
bob_home = start_app("bob", BOB_PORT)

song = os.path.join(ann_home, "Beats", "Night Drive")
os.makedirs(song)
if flp:
    shutil.copy(flp, os.path.join(song, "Night Drive.flp"))
else:
    open(os.path.join(song, "Night Drive.flp"), "wb").write(b"v1")

try:
    time.sleep(1.5)
    with sync_playwright() as p:
        browser = p.chromium.launch(executable_path="/usr/bin/google-chrome", args=["--no-sandbox"])
        errors = []
        ann = browser.new_page(viewport={"width": 1240, "height": 1000})
        bob = browser.new_page(viewport={"width": 1240, "height": 1000})
        for page, who in ((ann, "ann"), (bob, "bob")):
            page.on("pageerror", lambda e, who=who: errors.append(f"{who}: {e}"))
            page.on("dialog", lambda d: d.accept())
        ann.goto(f"http://127.0.0.1:{ANN_PORT}/")
        bob.goto(f"http://127.0.0.1:{BOB_PORT}/")
        ann.wait_for_selector("#myName")
        bob.wait_for_selector("#myName")

        # --- two clicks to a session ---------------------------------------
        slow_type(ann, "#myName", "vadym", "ann ")
        ann.click("#startSession")
        ann.wait_for_selector("#sessionCode", timeout=15000)
        code = ann.inner_text("#sessionCode")
        check("a session code is shown", code.startswith("HEN") and len(code) > 30)
        check("ann is signed in under her name", "vadym on" in ann.inner_text("#who"))
        time.sleep(4.5)
        check("the code stays the same across refreshes", ann.inner_text("#sessionCode") == code)

        # --- adding a project: the folder is enough ------------------------
        slow_type(ann, "#newFolder", song, "ann ")
        ann.keyboard.press("Enter")
        wait("the project to sync", lambda: "Up to date" in ann.inner_text("#projects"))
        check("the project is named after its folder", "Night Drive" in ann.inner_text("#projects"))
        check("the add field is emptied", ann.input_value("#newFolder") == "")
        check("the example folder shows its backslashes", "C:\\Users\\you\\Documents" in ann.get_attribute("#newFolder", "placeholder"))

        # A wrong press shows an error and keeps what was typed.
        slow_type(ann, "#newFolder", os.path.join(root, "does not exist"), "ann ")
        ann.click("#create")
        wait("the error line in the console", lambda: "create failed" in ann.inner_text("#lines"))
        check("a failed press keeps the typed folder", ann.input_value("#newFolder") != "")
        ann.fill("#newFolder", "")

        # --- the friend joins with the code --------------------------------
        slow_type(bob, "#myName", "max", "bob ")
        bob.fill("#joinCode", "not a code")
        bob.click("#joinSession")
        wait("the refusal of a wrong code", lambda: "connect failed" in bob.inner_text("#lines"))
        check("a wrong code does not sign the friend in", bob.is_visible("#myName"))
        bob.fill("#joinCode", "  " + code.lower() + "\n")
        bob.click("#joinSession")
        bob.wait_for_selector("#projects", timeout=20000)
        check("the friend is in the session", "max on" in bob.inner_text("#who"))
        bob_song = os.path.join(bob_home, "Documents", "Henosis", "Night Drive")
        wait("the project arriving at the friend without any click",
             file_is(os.path.join(bob_song, "Night Drive.flp"), open(os.path.join(song, "Night Drive.flp"), "rb").read()))
        wait("both names listed in the session", lambda: "max" in ann.inner_text("#people") and "vadym" in ann.inner_text("#people"))

        # --- the friend's own project travels back -------------------------
        bob_own = os.path.join(bob_home, "Documents", "Image-Line", "FL Studio", "Projects", "max beat")
        os.makedirs(bob_own)
        open(os.path.join(bob_own, "max beat.flp"), "wb").write(b"beat")
        wait("the friend's project being offered for one click", lambda: "max beat" in bob.inner_text("#suggest"), 30)
        bob.click("#suggest button[data-suggest]")
        wait("the friend's project arriving at ann", file_is(os.path.join(ann_home, "Documents", "Henosis", "max beat", "max beat.flp"), b"beat"))

        # --- history: a note on the next save, then comments ---------------
        # Clicking a project card picks the project the sidebar shows.
        bob.click('#projects [data-select]:has-text("Night Drive")')
        check("the friend's sidebar follows the clicked project", "Night Drive" in bob.inner_text("#histTitle"))
        wait("the first version in the sidebar", lambda: "Added Night Drive.flp" in ann.inner_text("#versions"))
        slow_type(ann, "#noteInput", "new bassline, check the drop", "ann ")
        ann.click("#setNote")
        wait("the note being shown", lambda: "new bassline" in ann.inner_text("#noteNow"))
        open(os.path.join(song, "Night Drive.flp"), "wb").write(b"v2 with bass")
        wait("the save appearing with its note", lambda: "new bassline, check the drop" in ann.inner_text("#versions"))
        check("the older version is still listed", "Added Night Drive.flp" in ann.inner_text("#versions"))
        check("the note was used up", "describes the change itself" in ann.inner_text("#noteNow"))

        # The friend sees the same history and comments on it.
        wait("the note in the friend's history", lambda: "new bassline, check the drop" in bob.inner_text("#versions"), 30)
        slow_type(bob, "#commentInput", "drop is fire", "bob ")
        bob.click("#sendComment")
        wait("the comment under its version", lambda: "drop is fire" in bob.inner_text("#versions"))
        wait("the comment reaching ann", lambda: "drop is fire" in ann.inner_text("#versions"), 30)
        check("the comment names its author", "max" in ann.inner_text("#versions"))
        check("the comment field is emptied", bob.input_value("#commentInput") == "")
        # A comment on an older version: pick it, then write.
        ann.click("#versions .ver:nth-child(2) button[data-comment-on]")
        check("the comment goes to the chosen version", "version 1" in ann.inner_text("#commentLabel"))
        slow_type(ann, "#commentInput", "this one was already good", "ann ")
        ann.click("#sendComment")
        wait("the second comment", lambda: "this one was already good" in ann.inner_text("#versions"))
        text = ann.inner_text("#versions")
        check("each comment sits under its own version", text.index("drop is fire") < text.index("this one was already good"))
        time.sleep(4.5)
        check("the history stays put across refreshes", "drop is fire" in ann.inner_text("#versions"))

        # --- the console can be made smaller and stays that way -------------
        before = ann.evaluate("document.querySelector('#lines').getBoundingClientRect().height")
        grip = ann.locator("#grip").bounding_box()
        ann.mouse.move(grip["x"] + grip["width"] / 2, grip["y"] + 3)
        ann.mouse.down()
        ann.mouse.move(grip["x"] + grip["width"] / 2, grip["y"] + 103, steps=10)
        ann.mouse.up()
        smaller = ann.evaluate("document.querySelector('#lines').getBoundingClientRect().height")
        check("dragging the strip makes the console smaller", smaller < before - 50)
        ann.reload()
        ann.wait_for_selector("#lines")
        time.sleep(1)
        kept = ann.evaluate("document.querySelector('#lines').getBoundingClientRect().height")
        check("the console keeps its size after a restart", abs(kept - smaller) < 5)

        # --- removing a project from the session -----------------------------
        bob_project = os.path.join(bob_home, "Documents", "Image-Line", "FL Studio", "Projects", "max beat", "max beat.flp")
        ann_copy = os.path.join(ann_home, "Documents", "Henosis", "max beat", "max beat.flp")
        check("the friend's project is on both computers", os.path.exists(bob_project) and os.path.exists(ann_copy))
        check("only its owner may remove it", ann.locator('[data-select]:has-text("max beat") button[data-act="delete"]').count() == 0)
        bob.click('#projects [data-select]:has-text("max beat") button[data-act="delete"]')
        wait("the project disappearing at the friend's", lambda: "max beat" not in bob.inner_text("#projects"))
        wait("ann being told it is gone", lambda: "No longer available" in ann.inner_text("#projects"))
        check("no files were deleted", os.path.exists(bob_project) and os.path.exists(ann_copy))
        check("it is not offered for download again", "max beat" not in ann.inner_text("#unlinked"))
        ann.click('#projects [data-select]:has-text("max beat")')
        check("the history says the project is gone", "not in the session any more" in ann.inner_text("#histBody"))
        ann.click('#projects [data-select]:has-text("max beat") button[data-act="unlink"]')
        wait("the gone project leaving ann's list", lambda: "max beat" not in ann.inner_text("#projects"))
        check("ann's copy of the files is still there", os.path.exists(ann_copy))

        # --- the debug console ---------------------------------------------
        console = ann.inner_text("#lines")
        check("the console shows what was pressed", "> host/start" in console and "> create folder=" in console)
        check("the console shows what the app did", "Night Drive: Uploaded your changes" in console)
        check("the friend's console shows the connection", "connect: found the session at" in bob.inner_text("#lines"))
        ann.check("#detail")
        time.sleep(4.5)
        check("the details switch stays on across refreshes", ann.is_checked("#detail"))
        check("details show the requests", "POST /v1/projects" in ann.inner_text("#lines"))
        both = ann.inner_text("#lines") + bob.inner_text("#lines")
        check("no token or session code in the consoles", "hn_" not in both and code[:20] not in both.upper())
        ann.click("#toggleLog")
        check("the console can be hidden", not ann.is_visible("#lines"))
        ann.click("#toggleLog")

        # --- a change on disk needs no click --------------------------------
        open(os.path.join(song, "kick.wav"), "wb").write(b"kick")
        wait("the new file reaching the friend", file_is(os.path.join(bob_song, "kick.wav"), b"kick"), 30)
        check("no script errors in either window", errors == [])

        here = os.path.dirname(os.path.abspath(__file__))
        ann.screenshot(path=os.path.join(here, "last-run-host.png"))
        bob.screenshot(path=os.path.join(here, "last-run-friend.png"))
        bob.goto(f"http://127.0.0.1:{BOB_PORT}/")   # the first screen, for the record
        browser.close()
finally:
    for app in apps:
        app.terminate()
    for app in apps:
        app.wait(timeout=10)
    shutil.rmtree(root, ignore_errors=True)

print("\n%d problem(s)" % len(failures))
sys.exit(1 if failures else 0)
