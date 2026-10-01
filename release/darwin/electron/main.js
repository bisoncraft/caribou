// Electron shell for the macOS desktop release.
//
// It spawns the bundled Go `caribou` binary (wallet core plus appserver),
// waits for the appserver to announce its address on stdout, and shows the
// web UI in a BrowserWindow. Linux and Windows ship the Go webview app in
// wallet/cmd/caribou-desktop instead; the macOS port of that window model was
// never completed, so this shell stands in for it.

const {
  app,
  BrowserWindow,
  Notification,
  ipcMain,
  Menu,
  Tray,
  shell,
  nativeImage,
  dialog,
  screen,
} = require("electron");
const { spawn } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");
const readline = require("node:readline");

const homepage = "https://github.com/bisoncraft/caribou";
const issues = "https://github.com/bisoncraft/caribou/issues";

let tray = null; // held globally so it is not garbage collected
let mainWindow = null;
let caribou = null;
let webURL = ""; // set once the appserver reports it is listening
let network = ""; // mainnet/testnet/simnet, from the startup log line
let shuttingDown = false;

// The desktop UI's layout floor is 1024 px wide.
const minWidth = 1024;
const minHeight = 600;

// Chromium's profile (cookies, local storage, caches) gets a directory of its
// own. The default, ~/Library/Application Support/Caribou, is the Go wallet's
// data directory. Set before the single-instance lock, which lives there.
app.setPath("userData", path.join(app.getPath("appData"), "Caribou", "electron"));

// A second launch focuses the running instance instead of starting another
// wallet process.
if (!app.requestSingleInstanceLock()) {
  app.quit();
} else {
  app.on("second-instance", showWindow);
  main();
}

function main() {
  app.whenReady().then(async () => {
    if (await offerMoveToApplications()) return; // relaunching from the new location
    startCaribou();
    Menu.setApplicationMenu(buildMenu());
    setTray();
    app.dock.setMenu(
      Menu.buildFromTemplate([{ label: "Open Logs Folder", click: openLogs }]),
    );
    app.on("activate", showWindow);
    showWindow(); // no-op until the appserver is up
  });

  // Keep running with only the tray and dock icon when the window closes;
  // the wallet may have work in flight.
  app.on("window-all-closed", () => {});

  app.on("before-quit", (e) => {
    // The app ends with app.exit once caribou is down, which destroys the
    // window without a close event.
    if (mainWindow && !mainWindow.isDestroyed()) saveBounds(mainWindow.getNormalBounds());
    if (caribou && caribou.exitCode === null && !shuttingDown) {
      e.preventDefault();
      shuttingDown = true;
      // caribou logs out and exits on SIGINT; its exit handler ends the app.
      caribou.kill("SIGINT");
      setTimeout(() => {
        if (caribou.exitCode === null) caribou.kill("SIGKILL");
      }, 15000);
    }
  });

  ipcMain.on("notify", (_, { title, body }) => {
    new Notification({ title, body }).show();
  });
  ipcMain.on("openUrl", (_, url) => openExternal(url));
}

// resourcesDir is Contents/Resources/mac in the packaged app, or whatever
// CARIBOU_RESOURCES names when running unpackaged from a checkout.
function resourcesDir() {
  return process.env.CARIBOU_RESOURCES || path.join(process.resourcesPath, "mac");
}

// offerMoveToApplications handles a launch from outside the Applications
// folder, typically a double-click on the app inside the mounted dmg: it
// offers to move the bundle to /Applications, which relaunches the app from
// there. Resolves to true when that relaunch is under way.
async function offerMoveToApplications() {
  if (process.env.CARIBOU_RESOURCES || app.isInApplicationsFolder()) return false;
  const state = readState();
  if (state.keepOutsideApplications) return false;
  const bundle = path.resolve(app.getPath("exe"), "..", "..", "..");
  // The sync variant returns only the button index; the checkbox state
  // comes with the async one.
  const { response, checkboxChecked } = await dialog.showMessageBox({
    type: "question",
    message: "Move Caribou to the Applications folder?",
    detail: `Caribou is running from ${path.dirname(bundle)}. Installed apps belong in the Applications folder.`,
    buttons: ["Move to Applications", "Not Now"],
    defaultId: 0,
    cancelId: 1,
    checkboxLabel: "Don't ask again",
  });
  if (checkboxChecked) writeState({ ...state, keepOutsideApplications: true });
  if (response !== 0) return false;
  return app.moveToApplicationsFolder({
    conflictHandler: (type) => {
      if (type === "existsAndRunning") {
        dialog.showErrorBox(
          "Caribou is already running",
          "Quit the Caribou in the Applications folder, then try again.",
        );
        return false;
      }
      const replace = dialog.showMessageBoxSync({
        type: "question",
        message: "Replace the Caribou already in the Applications folder?",
        detail: "The existing copy goes to the Trash. Wallet data is kept separately and is not affected.",
        buttons: ["Replace", "Cancel"],
        defaultId: 0,
        cancelId: 1,
      });
      return replace === 0;
    },
  });
}

function startCaribou() {
  const bin = path.join(resourcesDir(), "caribou");
  caribou = spawn(bin, [], { stdio: ["pipe", "pipe", "pipe"] });

  readline.createInterface({ input: caribou.stdout }).on("line", (line) => {
    console.log(line);
    // The appserver prints its address once it is listening (see
    // wallet/appserver/appserver.go). Parsing it rather than assuming the
    // default port respects a webaddr set in caribou.conf.
    const addr = line.match(/LOGIN AND TRADE\s+--->\s+(https?:\/\/\S+)/);
    if (addr && !webURL) {
      webURL = addr[1];
      if (app.isReady()) showWindow();
    }
    const net = line.match(/starting for network: (\w+)/);
    if (net && !network) network = net[1];
  });
  caribou.stderr.on("data", (data) => console.error(data.toString()));

  caribou.on("error", (err) => {
    dialog.showErrorBox("Caribou failed to start", err.message);
    app.exit(1);
  });
  caribou.on("exit", (code, signal) => {
    console.log(`caribou exited (code ${code}, signal ${signal})`);
    if (!shuttingDown && code) {
      dialog.showErrorBox(
        "Caribou stopped",
        `The wallet process exited unexpectedly (code ${code}). The logs folder has details.`,
      );
    }
    app.exit(code || 0);
  });
}

function showWindow() {
  if (!webURL || shuttingDown) return;
  if (mainWindow && !mainWindow.isDestroyed()) {
    mainWindow.show();
    mainWindow.focus();
    return;
  }
  mainWindow = new BrowserWindow({
    ...initialBounds(),
    minWidth,
    minHeight,
    show: false,
    backgroundColor: "#17130d", // the UI's default (warm) theme background
    webPreferences: {
      preload: path.join(__dirname, "preload.js"),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
    },
  });
  mainWindow.once("ready-to-show", () => mainWindow.show());
  mainWindow.on("close", () => saveBounds(mainWindow.getNormalBounds()));
  mainWindow.on("closed", () => {
    mainWindow = null;
  });
  // Links that would open a new window go to the default browser instead.
  mainWindow.webContents.setWindowOpenHandler(({ url }) => {
    openExternal(url);
    return { action: "deny" };
  });
  // Navigation stays on the appserver. Anything else goes to the default
  // browser rather than replacing the wallet UI in a window with no back
  // button.
  mainWindow.webContents.on("will-navigate", (e) => {
    if (new URL(e.url).origin !== new URL(webURL).origin) {
      e.preventDefault();
      openExternal(e.url);
    }
  });
  mainWindow.loadURL(webURL);
}

// The shell's own state (window bounds, dismissed prompts) lives in one JSON
// file under userData.
function stateFile() {
  return path.join(app.getPath("userData"), "state.json");
}

function readState() {
  try {
    return JSON.parse(fs.readFileSync(stateFile(), "utf8"));
  } catch {
    return {}; // first run, or an unreadable file: start over
  }
}

function writeState(state) {
  try {
    fs.mkdirSync(path.dirname(stateFile()), { recursive: true });
    fs.writeFileSync(stateFile(), JSON.stringify(state));
  } catch (err) {
    console.error(`saving ${stateFile()}: ${err.message}`);
  }
}

// initialBounds returns the window bounds saved at the last close if they are
// still mostly on a connected display, and otherwise 1280x820 centered on the
// display under the pointer, shrunk to fit its work area. A 13-inch MacBook's
// work area is about 1440x800 points with the Dock showing.
function initialBounds() {
  const b = readState().windowBounds;
  if (b && [b.x, b.y, b.width, b.height].every(Number.isFinite)) {
    const wa = screen.getDisplayMatching(b).workArea;
    const visibleW = Math.min(b.x + b.width, wa.x + wa.width) - Math.max(b.x, wa.x);
    const visibleH = Math.min(b.y + b.height, wa.y + wa.height) - Math.max(b.y, wa.y);
    if (b.width >= minWidth && b.height >= minHeight && visibleW >= 200 && visibleH >= 200) {
      return b;
    }
  }
  const wa = screen.getDisplayNearestPoint(screen.getCursorScreenPoint()).workArea;
  const width = Math.max(minWidth, Math.min(1280, wa.width));
  const height = Math.max(minHeight, Math.min(820, wa.height));
  return {
    x: wa.x + Math.round((wa.width - width) / 2),
    y: wa.y + Math.round((wa.height - height) / 2),
    width,
    height,
  };
}

function saveBounds(windowBounds) {
  writeState({ ...readState(), windowBounds });
}

function openExternal(url) {
  if (/^https?:\/\//.test(url)) shell.openExternal(url);
}

// openLogs opens the wallet's log directory in Finder. The Go side keeps its
// data under ~/Library/Application Support/Caribou/<network>/.
function openLogs() {
  shell.openPath(
    path.join(app.getPath("appData"), "Caribou", network || "mainnet", "logs"),
  );
}

function setTray() {
  // A template image (black plus alpha, loaded with its @2x variant), which
  // macOS tints to suit a light or dark menu bar.
  const icon = nativeImage.createFromPath(path.join(resourcesDir(), "caribouTemplate.png"));
  icon.setTemplateImage(true);
  tray = new Tray(icon);
  tray.setToolTip("Caribou");
  tray.setContextMenu(
    Menu.buildFromTemplate([
      { label: "Open Caribou", click: showWindow },
      { label: "Open Logs Folder", click: openLogs },
      { type: "separator" },
      { label: "Quit Caribou", accelerator: "CmdOrCtrl+Q", click: () => app.quit() },
    ]),
  );
  tray.on("click", showWindow);
}

function buildMenu() {
  return Menu.buildFromTemplate([
    { role: "appMenu" },
    { label: "File", submenu: [{ role: "close" }] },
    {
      label: "Edit",
      submenu: [
        { role: "undo" },
        { role: "redo" },
        { type: "separator" },
        { role: "cut" },
        { role: "copy" },
        { role: "paste" },
        { role: "pasteAndMatchStyle" },
        { role: "delete" },
        { role: "selectAll" },
      ],
    },
    { role: "viewMenu" },
    {
      label: "Window",
      submenu: [
        { label: "Show Caribou", accelerator: "CmdOrCtrl+Shift+O", click: showWindow },
        { role: "minimize" },
        { role: "zoom" },
        { type: "separator" },
        { role: "front" },
      ],
    },
    {
      role: "help",
      submenu: [
        { label: "Open Logs Folder", accelerator: "Cmd+Alt+L", click: openLogs },
        { type: "separator" },
        { label: "Website", click: () => shell.openExternal(homepage) },
        { label: "Report an Issue", click: () => shell.openExternal(issues) },
      ],
    },
  ]);
}
