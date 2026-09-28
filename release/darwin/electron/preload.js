const { contextBridge, ipcRenderer } = require("electron");

// Bridge for the web UI, matching the window.* names the UI declares in
// wallet/appserver/shared/src/registry.ts.
contextBridge.exposeInMainWorld("electron", {
  sendOSNotification: (title, body) => ipcRenderer.send("notify", { title, body }),
  openUrl: (url) => ipcRenderer.send("openUrl", url),
});
