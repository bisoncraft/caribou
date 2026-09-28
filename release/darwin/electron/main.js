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
} = require("electron");
const { spawn } = require("node:child_process");
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

// A second launch focuses the running instance instead of starting another
// wallet process.
if (!app.requestSingleInstanceLock()) {
  app.quit();
} else {
  app.on("second-instance", showWindow);
  main();
}

function main() {
  startCaribou();

  app.whenReady().then(() => {
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
    width: 1280,
    height: 820,
    minWidth: 1024,
    minHeight: 600,
    show: false,
    backgroundColor: "#0b1218",
    webPreferences: {
      preload: path.join(__dirname, "preload.js"),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
    },
  });
  mainWindow.once("ready-to-show", () => mainWindow.show());
  mainWindow.on("closed", () => {
    mainWindow = null;
  });
  // Links that would open a new window go to the default browser instead.
  mainWindow.webContents.setWindowOpenHandler(({ url }) => {
    openExternal(url);
    return { action: "deny" };
  });
  mainWindow.loadURL(webURL);
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
  const icon = nativeImage.createFromPath(path.join(resourcesDir(), "caribou-16.png"));
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
