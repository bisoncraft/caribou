; Windows installer for the desktop release, built on Linux by
; release/package-windows.sh with makensis (NSIS 3.08 or later):
;
;   cd <stage dir> && makensis -NOCD -DVERSION=<semver> -DVERSION_NUM=<x.y.z> \
;     -DOUTFILE=<setup.exe> -DICON=<caribou.ico> [-DXMR] [-DSIGN_CMD=<command>] \
;     /release/windows/caribou.nsi
;
; File sources resolve against the stage directory (-NOCD keeps it the working
; directory). ICON is the program's own .ico (wallet/cmd/caribou-desktop/src),
; so the installer and the program share one icon. SIGN_CMD, when defined,
; runs on the uninstaller as NSIS writes it and on the finished installer, so
; both carry an Authenticode signature; a non-zero exit fails the build.
;
; The install is per-machine, into Program Files. Wallet data lives in the
; user's profile (%LOCALAPPDATA%\Caribou) and is never touched by the
; installer or the uninstaller.

Unicode true
ManifestDPIAware true
SetCompressor /SOLID lzma

!include "MUI2.nsh"
!include "x64.nsh"
!include "LogicLib.nsh"
!include "FileFunc.nsh"

!define APPNAME "Caribou"
!define EXE "caribou-desktop.exe"
!define PUBLISHER "The Caribou Developers"
!define HOMEPAGE "https://github.com/bisoncraft/caribou"
!define UNINSTKEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\Caribou"
; The Evergreen WebView2 Runtime's client ID, the same on every machine.
!define WEBVIEW2_CLIENT "{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}"

!ifdef SIGN_CMD
!uninstfinalize '${SIGN_CMD} "%1"' = 0
!finalize '${SIGN_CMD} "%1"' = 0
!endif

Name "${APPNAME}"
OutFile "${OUTFILE}"
InstallDir "$PROGRAMFILES64\${APPNAME}"
InstallDirRegKey HKLM "${UNINSTKEY}" "InstallLocation"
RequestExecutionLevel admin
BrandingText "${APPNAME} ${VERSION}"

VIProductVersion "${VERSION_NUM}.0"
VIAddVersionKey "ProductName" "${APPNAME}"
VIAddVersionKey "CompanyName" "${PUBLISHER}"
VIAddVersionKey "LegalCopyright" "© ${PUBLISHER}. Blue Oak Model License 1.0.0"
VIAddVersionKey "FileDescription" "${APPNAME} installer"
VIAddVersionKey "FileVersion" "${VERSION_NUM}.0"
VIAddVersionKey "ProductVersion" "${VERSION}"

!define MUI_ICON "${ICON}"
!define MUI_UNICON "${ICON}"
!define MUI_ABORTWARNING

!define MUI_FINISHPAGE_RUN ""
!define MUI_FINISHPAGE_RUN_TEXT "Open ${APPNAME}"
!define MUI_FINISHPAGE_RUN_FUNCTION LaunchApp
; The Modern UI finish page has two checkboxes; the "show readme" one carries
; the desktop shortcut option.
!define MUI_FINISHPAGE_SHOWREADME ""
!define MUI_FINISHPAGE_SHOWREADME_TEXT "Create a desktop shortcut"
!define MUI_FINISHPAGE_SHOWREADME_FUNCTION CreateDesktopShortcut

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "English"

Function .onInit
	${IfNot} ${RunningX64}
		MessageBox MB_ICONSTOP "${APPNAME} requires 64-bit Windows." /SD IDOK
		Abort
	${EndIf}
	SetRegView 64
	SetShellVarContext all

	; The app renders in the Edge WebView2 control, whose runtime ships with
	; Windows 11 and with Microsoft Edge on Windows 10. The Evergreen runtime
	; records its version under one of these keys; an absent or 0.0.0.0
	; version means it is not installed.
	ReadRegStr $0 HKLM "SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\${WEBVIEW2_CLIENT}" "pv"
	${If} $0 == ""
	${OrIf} $0 == "0.0.0.0"
		ReadRegStr $0 HKCU "Software\Microsoft\EdgeUpdate\Clients\${WEBVIEW2_CLIENT}" "pv"
	${EndIf}
	${If} $0 == ""
	${OrIf} $0 == "0.0.0.0"
		MessageBox MB_ICONSTOP "${APPNAME} needs the Microsoft Edge WebView2 Runtime, which comes with Windows 11 and Microsoft Edge. Install it from https://developer.microsoft.com/microsoft-edge/webview2/ and run this installer again." /SD IDOK
		Abort
	${EndIf}
FunctionEnd

Function un.onInit
	SetRegView 64
	SetShellVarContext all
FunctionEnd

; The installer runs elevated. Starting the app through Explorer runs it with
; the signed-in user's own token rather than the installer's administrator
; token.
Function LaunchApp
	Exec '"$WINDIR\explorer.exe" "$INSTDIR\${EXE}"'
FunctionEnd

; Caribou keeps running in the tray after its window closes, and Windows will
; not replace or delete a running program. A running image cannot be opened
; for writing, which is the test here. When it is running, setup offers to
; close it the way Quit in its tray menu does (caribou-desktop --kill, through
; the running instance's sync server), waits for it to exit, and otherwise
; asks the user to quit it and try again.
!macro DEFINE_CLOSE_APP PREFIX
Function ${PREFIX}CloseApp
	Push $0
	Push $1
	Push $2
	StrCpy $1 0 ; whether the close has been tried
	${Do}
		${IfNot} ${FileExists} "$INSTDIR\${EXE}"
			${Break}
		${EndIf}
		ClearErrors
		FileOpen $0 "$INSTDIR\${EXE}" a
		${IfNot} ${Errors}
			FileClose $0
			${Break}
		${EndIf}
		${If} $1 == 0
			StrCpy $1 1
			MessageBox MB_OKCANCEL|MB_ICONINFORMATION "${APPNAME} is running. Setup will close it, as Quit in its tray menu does, and then continue." /SD IDOK IDOK close
			Quit
		close:
			ExecWait '"$INSTDIR\${EXE}" --kill'
			StrCpy $2 0
			${Do}
				Sleep 500
				ClearErrors
				FileOpen $0 "$INSTDIR\${EXE}" a
				${IfNot} ${Errors}
					FileClose $0
					${Break}
				${EndIf}
				IntOp $2 $2 + 1
			${LoopUntil} $2 >= 60
		${Else}
			MessageBox MB_RETRYCANCEL|MB_ICONEXCLAMATION "${APPNAME} is still running. Quit it from its icon in the notification area, then click Retry." /SD IDCANCEL IDRETRY retry
			Quit
		retry:
		${EndIf}
	${Loop}
	Pop $2
	Pop $1
	Pop $0
FunctionEnd
!macroend
!insertmacro DEFINE_CLOSE_APP ""
!insertmacro DEFINE_CLOSE_APP "un."

Function CreateDesktopShortcut
	CreateShortcut "$DESKTOP\${APPNAME}.lnk" "$INSTDIR\${EXE}"
FunctionEnd

Section "Install"
	Call CloseApp
	SetOutPath "$INSTDIR"
	File "${EXE}"
	File "WebView2Loader.dll"
	!ifdef XMR
	File "libmonero_wallet2_api_c.dll"
	!endif

	WriteUninstaller "$INSTDIR\uninstall.exe"
	CreateShortcut "$SMPROGRAMS\${APPNAME}.lnk" "$INSTDIR\${EXE}"

	WriteRegStr HKLM "${UNINSTKEY}" "DisplayName" "${APPNAME}"
	WriteRegStr HKLM "${UNINSTKEY}" "DisplayVersion" "${VERSION}"
	WriteRegStr HKLM "${UNINSTKEY}" "Publisher" "${PUBLISHER}"
	WriteRegStr HKLM "${UNINSTKEY}" "URLInfoAbout" "${HOMEPAGE}"
	WriteRegStr HKLM "${UNINSTKEY}" "DisplayIcon" "$INSTDIR\${EXE},0"
	WriteRegStr HKLM "${UNINSTKEY}" "InstallLocation" "$INSTDIR"
	WriteRegStr HKLM "${UNINSTKEY}" "UninstallString" '"$INSTDIR\uninstall.exe"'
	WriteRegStr HKLM "${UNINSTKEY}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /S'
	WriteRegDWORD HKLM "${UNINSTKEY}" "NoModify" 1
	WriteRegDWORD HKLM "${UNINSTKEY}" "NoRepair" 1
	${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
	IntFmt $0 "0x%08X" $0
	WriteRegDWORD HKLM "${UNINSTKEY}" "EstimatedSize" $0
SectionEnd

Section "Uninstall"
	Call un.CloseApp
	Delete "$INSTDIR\${EXE}"
	Delete "$INSTDIR\WebView2Loader.dll"
	Delete "$INSTDIR\libmonero_wallet2_api_c.dll"
	Delete "$INSTDIR\uninstall.exe"
	RMDir "$INSTDIR"

	Delete "$SMPROGRAMS\${APPNAME}.lnk"
	Delete "$DESKTOP\${APPNAME}.lnk"
	DeleteRegKey HKLM "${UNINSTKEY}"
SectionEnd
