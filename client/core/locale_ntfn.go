package core

import (
	"fmt"

	"decred.org/dcrdex/client/intl"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

type translation struct {
	subject  intl.Translation
	template intl.Translation
}

const originLang = "en-US"

// originLocale is the American English translations.
var originLocale = map[Topic]*translation{
	TopicWalletConnectionWarning: {
		subject:  intl.Translation{T: "Wallet connection warning"},
		template: intl.Translation{T: "Incomplete registration detected for %s, but failed to connect to the Decred wallet", Notes: "args: [host]"},
	},
	TopicWalletUnlockError: {
		subject:  intl.Translation{T: "Wallet unlock error"},
		template: intl.Translation{T: "Connected to wallet to complete registration at %s, but failed to unlock: %v", Notes: "args: [host, error]"},
	},
	TopicWalletCommsWarning: {
		subject:  intl.Translation{T: "Wallet connection issue"},
		template: intl.Translation{T: "Unable to communicate with %v wallet! Reason: %v", Notes: "args: [asset name, error message]"},
	},
	TopicWalletPeersWarning: {
		subject:  intl.Translation{T: "Wallet network issue"},
		template: intl.Translation{T: "%v wallet has no network peers!", Notes: "args: [asset name]"},
	},
	TopicWalletPeersRestored: {
		subject:  intl.Translation{T: "Wallet connectivity restored"},
		template: intl.Translation{T: "%v wallet has reestablished connectivity.", Notes: "args: [asset name]"},
	},
	TopicSendError: {
		subject:  intl.Translation{T: "Send error"},
		template: intl.Translation{Version: 1, T: "Error encountered while sending %s: %v", Notes: "args: [ticker, error]"},
	},
	TopicSendSuccess: {
		subject:  intl.Translation{T: "Send successful"},
		template: intl.Translation{Version: 1, T: "Sending %s %s to %s has completed successfully. Tx ID = %s", Notes: "args: [value string, ticker, destination address, coin ID]"},
	},
	TopicWalletConfigurationUpdated: {
		subject:  intl.Translation{T: "Wallet configuration updated"},
		template: intl.Translation{T: "Configuration for %s wallet has been updated. Deposit address = %s", Notes: "args: [ticker, address]"},
	},
	TopicWalletPasswordUpdated: {
		subject:  intl.Translation{T: "Wallet Password Updated"},
		template: intl.Translation{T: "Password for %s wallet has been updated.", Notes: "args:  [ticker]"},
	},
	TopicWalletTypeDeprecated: {
		subject:  intl.Translation{T: "Wallet Disabled"},
		template: intl.Translation{T: "Your %s wallet type is no longer supported. Create a new wallet."},
	},
	TopicSeedNeedsSaving: {
		subject:  intl.Translation{T: "Don't forget to back up your application seed"},
		template: intl.Translation{T: "A new application seed has been created. Make a back up now in the settings view."},
	},
	TopicUpgradedToSeed: {
		subject:  intl.Translation{T: "Back up your new application seed"},
		template: intl.Translation{T: "The client has been upgraded to use an application seed. Back up the seed now in the settings view."},
	},
}

var ptBR = map[Topic]*translation{
	TopicWalletConnectionWarning: {
		subject:  intl.Translation{T: "Aviso de Conexão com a Carteira"},
		template: intl.Translation{T: "Registro incompleto detectado para %s, mas falhou ao conectar com carteira decred"},
	},
	TopicWalletUnlockError: {
		subject:  intl.Translation{T: "Erro ao Destravar Carteira"},
		template: intl.Translation{T: "Conectado com carteira para completar o registro em %s, mas falha ao destrancar: %v"},
	},
	TopicSendError: {
		subject:  intl.Translation{T: "Erro ao enviar"},
		template: intl.Translation{Version: 1, T: "Erro encontrado ao enviar %s: %v"},
	},
	TopicSendSuccess: {
		subject:  intl.Translation{T: "Envio realizado"},
		template: intl.Translation{Version: 1, T: "Envio de %s %s para %s concluído com sucesso. Tx ID = %s"},
	},
	TopicWalletConfigurationUpdated: {
		template: intl.Translation{T: "configuração para carteira %s foi atualizada. Endereço de depósito = %s"},
		subject:  intl.Translation{T: "Configurações da Carteira Atualizada"},
	},
	TopicWalletPasswordUpdated: {
		template: intl.Translation{T: "Senha para carteira %s foi atualizada."},
		subject:  intl.Translation{T: "Senha da Carteira Atualizada"},
	},
	TopicSeedNeedsSaving: {
		subject:  intl.Translation{T: "Não se esqueça de guardar a seed do app"},
		template: intl.Translation{T: "Uma nova seed para a aplicação foi criada. Faça um backup agora na página de configurações."},
	},
	TopicUpgradedToSeed: {
		subject:  intl.Translation{T: "Guardar nova seed do app"},
		template: intl.Translation{T: "O cliente foi atualizado para usar uma seed. Faça backup dessa seed na página de configurações."},
	},
	TopicWalletCommsWarning: {
		subject:  intl.Translation{T: "Problema de conexão da carteira"},
		template: intl.Translation{T: "Não foi possível comunicar com a carteira %v! Motivo: %v"},
	},
	TopicWalletPeersWarning: {
		subject:  intl.Translation{T: "Problema de rede da carteira"},
		template: intl.Translation{T: "A carteira %v não tem pares na rede!"},
	},
	TopicWalletPeersRestored: {
		subject:  intl.Translation{T: "Conectividade da carteira restaurada"},
		template: intl.Translation{T: "A carteira %v restabeleceu a conectividade."},
	},
	TopicWalletTypeDeprecated: {
		subject:  intl.Translation{T: "Carteira desabilitada"},
		template: intl.Translation{T: "O tipo da sua carteira %s não é mais suportado. Crie uma nova carteira."},
	},
}

// zhCN is the Simplified Chinese (PRC) translations.
var zhCN = map[Topic]*translation{
	TopicWalletConnectionWarning: {
		subject:  intl.Translation{T: "钱包连接通知"},
		template: intl.Translation{T: "检测到 %s 的注册不完整，无法连接 decred 钱包"},
	},
	TopicWalletUnlockError: {
		subject:  intl.Translation{T: "解锁钱包时出错"},
		template: intl.Translation{T: "与 decred 钱包连接以在 %s 上完成注册，但无法解锁： %v"},
	},
	TopicSendError: {
		subject:  intl.Translation{T: "发送错误"},
		template: intl.Translation{Version: 1, T: "发送 %s 时遇到错误：%v"},
	},
	TopicSendSuccess: {
		subject:  intl.Translation{T: "发送成功"},
		template: intl.Translation{Version: 1, T: "已成功发送 %s %s 到 %s。Tx ID = %s"},
	},
	TopicWalletConfigurationUpdated: {
		subject:  intl.Translation{T: "更新的钱包设置a"},
		template: intl.Translation{T: "钱包 %[1]s 的配置已更新。存款地址 = %[2]s"},
	},
	TopicWalletPasswordUpdated: {
		subject:  intl.Translation{T: "钱包密码更新"},
		template: intl.Translation{T: "钱包 %s 的密码已更新。"},
	},
	TopicSeedNeedsSaving: {
		subject:  intl.Translation{T: "不要忘记备份你的应用程序种子"},
		template: intl.Translation{T: "已创建新的应用程序种子。请立刻在设置界面中进行备份。"},
	},
	TopicUpgradedToSeed: {
		subject:  intl.Translation{T: "备份您的新应用程序种子"},
		template: intl.Translation{T: "客户端已升级为使用应用程序种子。请切换至设置界面备份种子。"},
	},
	TopicWalletPeersWarning: {
		subject:  intl.Translation{T: "钱包网络问题"},
		template: intl.Translation{T: "%v 钱包没有网络对等节点！", Notes: "args: [asset name]"},
	},
	TopicWalletCommsWarning: {
		subject:  intl.Translation{T: "钱包连接问题"},
		template: intl.Translation{T: "无法与 %v 钱包通信！原因：%v", Notes: "args: [asset name, error message]"},
	},
	TopicWalletPeersRestored: {
		subject:  intl.Translation{T: "钱包连接已恢复"},
		template: intl.Translation{T: "%v 钱包已重新建立连接。", Notes: "args: [asset name]"},
	},
	TopicWalletTypeDeprecated: {
		subject:  intl.Translation{T: "钱包已禁用"},
		template: intl.Translation{T: "您的 %s 钱包类型不再受支持，请创建一个新钱包。"},
	},
}

var plPL = map[Topic]*translation{
	TopicWalletConnectionWarning: {
		subject:  intl.Translation{T: "Ostrzeżenie połączenia z portfelem"},
		template: intl.Translation{T: "Wykryto niedokończoną rejestrację dla %s, ale nie można połączyć się z portfelem Decred"},
	},
	TopicWalletUnlockError: {
		subject:  intl.Translation{T: "Błąd odblokowywania portfela"},
		template: intl.Translation{T: "Połączono z portfelem Decred, aby dokończyć rejestrację na %s, lecz próba odblokowania portfela nie powiodła się: %v"},
	},
	TopicSendError: {
		subject:  intl.Translation{T: "Błąd wypłaty środków"},
		template: intl.Translation{Version: 1, T: "Napotkano błąd przy wysyłaniu %s: %v"},
	},
	TopicSendSuccess: {
		subject:  intl.Translation{T: "Wypłata zrealizowana"},
		template: intl.Translation{Version: 1, T: "Wysyłka %s %s na adres %s została zakończona. Tx ID = %s"},
	},
	TopicWalletConfigurationUpdated: {
		subject:  intl.Translation{T: "Zaktualizowano konfigurację portfela"},
		template: intl.Translation{T: "Konfiguracja dla portfela %s została zaktualizowana. Adres do depozytów = %s"},
	},
	TopicWalletPasswordUpdated: {
		subject:  intl.Translation{T: "Zaktualizowano hasło portfela"},
		template: intl.Translation{T: "Hasło dla portfela %s zostało zaktualizowane."},
	},
	TopicSeedNeedsSaving: {
		subject:  intl.Translation{T: "Nie zapomnij zrobić kopii ziarna aplikacji"},
		template: intl.Translation{T: "Utworzono nowe ziarno aplikacji. Zrób jego kopię w zakładce ustawień."},
	},
	TopicUpgradedToSeed: {
		subject:  intl.Translation{T: "Zrób kopię nowego ziarna aplikacji"},
		template: intl.Translation{T: "Klient został zaktualizowany, by korzystać z ziarna aplikacji. Zrób jego kopię w zakładce ustawień."},
	},
	TopicWalletPeersRestored: {
		subject:  intl.Translation{T: "Przywrócono łączność z portfelem"},
		template: intl.Translation{T: "Portfel %v odzyskał połączenie."},
	},
	TopicWalletTypeDeprecated: {
		subject:  intl.Translation{T: "Portfel wyłączony"},
		template: intl.Translation{T: "Twój portfel %s nie jest już wspierany. Utwórz nowy portfel."},
	},
	TopicWalletCommsWarning: {
		subject:  intl.Translation{T: "Problem z połączeniem portfela"},
		template: intl.Translation{T: "Nie można połączyć się z portfelem %v! Powód: %v"},
	},
	TopicWalletPeersWarning: {
		subject:  intl.Translation{T: "Problem z siecią portfela"},
		template: intl.Translation{T: "Portfel %v nie ma połączeń z resztą sieci (peer)!"},
	},
}

// deDE is the German translations.
var deDE = map[Topic]*translation{
	TopicWalletConnectionWarning: {
		subject:  intl.Translation{T: "Warnung bei Wallet Verbindung"},
		template: intl.Translation{T: "Unvollständige Registration für %s erkannt, konnte keine Verbindung zum Decred Wallet herstellen"},
	},
	TopicWalletUnlockError: {
		subject:  intl.Translation{T: "Fehler beim Entsperren des Wallet"},
		template: intl.Translation{T: "Verbunden zum Wallet um die Registration bei %s abzuschließen, ein Fehler beim entsperren des Wallet ist aufgetreten: %v"},
	},
	TopicWalletCommsWarning: {
		subject:  intl.Translation{T: "Probleme mit der Verbindung zum Wallet"},
		template: intl.Translation{T: "Kommunikation mit dem %v Wallet nicht möglich! Grund: %v"},
	},
	TopicWalletPeersWarning: {
		subject:  intl.Translation{T: "Problem mit dem Wallet-Netzwerk"},
		template: intl.Translation{T: "%v Wallet hat keine Netzwerk-Peers!"},
	},
	TopicWalletPeersRestored: {
		subject:  intl.Translation{T: "Wallet-Konnektivität wiederhergestellt"},
		template: intl.Translation{T: "Die Verbindung mit dem %v Wallet wurde wiederhergestellt."},
	},
	TopicSendError: {
		subject:  intl.Translation{T: "Sendefehler"},
		template: intl.Translation{Version: 1, T: "Fehler beim Senden von %s: %v"},
	},
	TopicSendSuccess: {
		subject:  intl.Translation{T: "Erfolgreich gesendet"},
		template: intl.Translation{Version: 1, T: "Das Senden von %s %s an %s wurde erfolgreich abgeschlossen. Tx-ID = %s"},
	},
	TopicWalletConfigurationUpdated: {
		subject:  intl.Translation{T: "Aktualisierung der Wallet Konfiguration"},
		template: intl.Translation{T: "Konfiguration für Wallet %s wurde aktualisiert. Einzahlungsadresse = %s"},
	},
	TopicWalletPasswordUpdated: {
		subject:  intl.Translation{T: "Wallet-Passwort aktualisiert"},
		template: intl.Translation{T: "Passwort für das %s Wallet wurde aktualisiert."},
	},
	TopicSeedNeedsSaving: {
		subject:  intl.Translation{T: "Vergiss nicht deinen App-Seed zu sichern"},
		template: intl.Translation{T: "Es wurde ein neuer App-Seed erstellt. Erstelle jetzt eine Sicherungskopie in den Einstellungen."},
	},
	TopicUpgradedToSeed: {
		subject:  intl.Translation{T: "Sichere deinen neuen App-Seed"},
		template: intl.Translation{T: "Dein Klient wurde aktualisiert und nutzt nun einen App-Seed. Erstelle jetzt eine Sicherungskopie in den Einstellungen."},
	},
	TopicWalletTypeDeprecated: {
		subject:  intl.Translation{T: "Wallet Disabled"},
		template: intl.Translation{T: "Dein %s Wallet wird nicht länger unterstützt. Erstelle eine neues Wallet."},
	},
}

// ar is the Arabic translations.
var ar = map[Topic]*translation{
	TopicWalletConnectionWarning: {
		subject:  intl.Translation{T: "تحذير اتصال المحفظة"},
		template: intl.Translation{T: "تم الكشف عن تسجيل غير مكتمل لـ  \u200e%s، لكنه فشل في الاتصال بمحفظة ديكريد"},
	},
	TopicWalletUnlockError: {
		subject:  intl.Translation{T: "خطأ في فتح المحفظة"},
		template: intl.Translation{T: "متصل بالمحفظة لإكمال التسجيل عند \u200e%s، لكنه فشل في فتح القفل: \u200e%v"},
	},
	TopicWalletCommsWarning: {
		subject:  intl.Translation{T: "مشكلة الإتصال بالمحفظة"},
		template: intl.Translation{T: "غير قادر على الاتصال بمحفظة !\u200e%v السبب: \u200e%v"},
	},
	TopicWalletPeersWarning: {
		subject:  intl.Translation{T: "مشكلة في شبكة المحفظة"},
		template: intl.Translation{T: "!لا يوجد لدى المحفظة \u200e%v نظراء على الشبكة"},
	},
	TopicWalletPeersRestored: {
		subject:  intl.Translation{T: "تمت استعادة الاتصال بالمحفظة"},
		template: intl.Translation{T: "تمت اعادة الاتصال بالمحفظة \u200e%v."},
	},
	TopicSendError: {
		subject:  intl.Translation{T: "إرسال الخطأ"},
		template: intl.Translation{Version: 1, T: "حدث خطأ أثناء إرسال %s: %v"},
	},
	TopicSendSuccess: {
		subject:  intl.Translation{T: "تم الإرسال بنجاح"},
		template: intl.Translation{Version: 1, T: "تم إرسال %s %s إلى %s بنجاح. معرف المعاملة Tx ID = %s"},
	},
	TopicWalletConfigurationUpdated: {
		subject:  intl.Translation{T: "تم تحديث تهيئة المحفظة"},
		template: intl.Translation{T: "تم تحديث تهيئة المحفظة \u200e%s. عنوان الإيداع = \u200e%s"},
	},
	TopicWalletPasswordUpdated: {
		subject:  intl.Translation{T: "تم تحديث كلمة مرور المحفظة"},
		template: intl.Translation{T: "تم تحديث كلمة المرور لمحفظة \u200e%s."},
	},
	TopicSeedNeedsSaving: {
		subject:  intl.Translation{T: "لا تنس عمل نسخة احتياطية من بذرة التطبيق"},
		template: intl.Translation{T: "تم إنشاء بذرة تطبيق جديدة. قم بعمل نسخة احتياطية الآن في عرض الإعدادات."},
	},
	TopicUpgradedToSeed: {
		subject:  intl.Translation{T: "قم بعمل نسخة احتياطية من بذرة التطبيق الجديدة"},
		template: intl.Translation{T: "تم تحديث العميل لاستخدام بذرة التطبيق. قم بعمل نسخة احتياطية من البذرة الآن في عرض الإعدادات."},
	},
	TopicWalletTypeDeprecated: {
		subject:  intl.Translation{T: "المحفظة معطلة"},
		template: intl.Translation{T: "لم يعد نوع محفظتك %s مدعوماً. قم بإنشاء محفظة جديدة."},
	},
}

// The language string key *must* parse with language.Parse.
var locales = map[string]map[Topic]*translation{
	originLang: originLocale,
	"pt-BR":    ptBR,
	"zh-CN":    zhCN,
	"pl-PL":    plPL,
	"de-DE":    deDE,
	"ar":       ar,
}

func init() {
	for lang, translations := range locales {
		langtag, err := language.Parse(lang)
		if err != nil {
			panic(err.Error())
		} // otherwise would fail in core.New parsing the languages
		for topic, translation := range translations {
			err := message.SetString(langtag, string(topic), translation.template.T)
			if err != nil {
				panic(fmt.Sprintf("SetString(%s): %v", lang, err))
			}
		}
	}
}

// RegisterTranslations registers translations with the init package for
// translator worksheet preparation.
func RegisterTranslations() {
	const callerID = "notifications"

	for lang, m := range locales {
		r := intl.NewRegistrar(callerID, lang, len(m)*2)
		for topic, t := range m {
			r.Register(string(topic)+" subject", &t.subject)
			r.Register(string(topic)+" template", &t.template)
		}
	}
}

// CheckTopicLangs is used to report missing notification translations.
func CheckTopicLangs() (missingTranslations int) {
	for topic := range originLocale {
		for _, m := range locales {
			if _, found := m[topic]; !found {
				missingTranslations += len(m)
			}
		}
	}
	return
}
