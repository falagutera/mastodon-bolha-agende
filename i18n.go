package main

import (
	"net/http"
	"slices"
	"strings"
)

const langCookie = "__Host-agende-lang"

var translations = map[string]map[string]string{
	"en": {
		"lang": "en", "nav_notes": "Release notes", "notes_title": "Release notes", "notes_lede": "What changed in each version.", "language": "Language", "title": "Agende",
		"lede":      "Schedule your posts on %s. The instance stores and publishes them, so they go out even if this site is down.",
		"login":     "Sign in with your account",
		"privacy":   "Your access token stays encrypted during your session and is revoked when you sign out or after 8 hours. Posts stay on the instance. Your unsent draft stays unencrypted in this browser for up to 7 days, or until you schedule it or sign out. Your settings and a log of your actions are kept here, with up to 80 encrypted characters of the text, even from private posts.",
		"nav_queue": "Queue", "nav_calendar": "Calendar", "nav_new": "New post", "nav_settings": "Settings", "logout": "Sign out",
		"date_format": "Jan 2, 2006 15:04", "day_format": "Jan 2, 2006",
		"weekdays": "Sun,Mon,Tue,Wed,Thu,Fri,Sat",
		"months":   "January,February,March,April,May,June,July,August,September,October,November,December",
		"hours":    "hours", "minutes": "minutes",
		"queue_title": "Scheduled posts", "queue_empty": "Nothing scheduled yet.", "remaining": "slots left of 300",
		"timezone_note": "Times in", "open": "Open", "duplicate": "Duplicate", "cancel": "Cancel post", "cancel_confirm": "Canceling can't be undone.",
		"has_media": "media", "local_only": "Only on this instance", "expires": "Self-destructs", "markdown": "Markdown",
		"cal_month": "Month", "cal_week": "Week", "cal_prev": "Previous", "cal_next": "Next", "no_posts": "No posts",
		"new_title": "New post", "text": "Text", "cw": "Content warning", "visibility": "Visibility",
		"vis_public": "Public", "vis_unlisted": "Quiet public", "vis_private": "Followers", "vis_direct": "Mentioned people",
		"post_language": "Post language", "reply": "Reply to (post URL or id, optional)", "plain": "Send as plain text",
		"attachment": "File", "alt": "Image description", "when": "Date and time", "timezone": "Time zone",
		"expiry": "Self-destruct after publishing", "never": "Never", "repeat": "Repeat", "once": "Once", "daily": "Every day", "weekly": "Every week", "monthly": "Every month",
		"times": "How many posts", "dates_preview": "Dates", "schedule": "Schedule", "preview": "Preview (approximate)", "characters": "characters left",
		"result_title": "Series result", "result_ok": "Scheduled", "result_no": "Not scheduled", "back_queue": "Back to the queue",
		"post_title": "Scheduled post", "edit": "Edit", "edit_title": "Edit post", "keep_file": "Keep this file", "flash_edited": "Post updated.", "flash_edited_all": "Post and the later ones with the same text updated.", "siblings_found": "%d later posts have the same text", "scope_one": "Change only this one", "scope_future": "Change this one and the %d later ones", "scope_note": "Each later post keeps its own date and files. New files go only to this one.", "form_later_partial": "This post was updated, but some later ones couldn't be. They are marked below and still have the old text.", "edit_blocked": "This post has a poll or quote from another app, so only its date can change here.", "form_edit_failed": "Couldn't save. The original post is still scheduled, unchanged.", "form_old_not_cancelled": "The new version is scheduled, but the original couldn't be canceled. Check the queue and cancel it, or both will go out.", "error_not_editable": "This post has a poll or quote from another app and can't be edited here.", "change_date": "Change date", "save": "Save", "edit_note": "Mastodon only lets the date change. To change the text, duplicate the post and cancel this one.",
		"settings_title": "Settings", "default_markdown": "Write in Markdown by default", "default_visibility": "Default visibility", "default_language": "Default post language",
		"flash_created": "Post scheduled.", "flash_rescheduled": "Date changed.", "flash_cancelled": "Post canceled.", "flash_cancelled_many": "Posts canceled.", "form_none_selected": "No post selected.", "form_cancel_partial": "Some posts were canceled, but then the instance stopped answering. What is still listed below was not canceled.", "select_all": "Select all", "cancel_selected": "Cancel selected", "cancel_many_confirm": "Cancel the selected posts? This can't be undone.", "select_post": "Select", "cancel_scope_one": "Cancel only this one", "cancel_scope_future": "Cancel this one and the %d later ones with the same text", "flash_saved": "Settings saved.",
		"form_invalid": "Some fields are invalid.", "form_empty": "Write something or attach a file.", "form_too_soon": "Pick a time at least 6 minutes from now.",
		"form_daily_limit": "That day already has the 25 posts Mastodon allows.", "form_total_limit": "That goes over the 300 scheduled posts Mastodon allows.",
		"form_bad_date": "Pick a valid date.", "form_bad_series": "A series has 2 to 24 posts.", "form_bad_reply": "Use the id or a post URL from this instance.",
		"form_bad_media": "That file type isn't accepted.", "form_too_many_media": "Too many files.", "form_too_many_uploads": "Too many files for this series: 25 uploads at most.",
		"form_bad_timezone": "Unknown time zone. Pick one from the list, or use a name like America/Sao_Paulo.", "form_stopped": "Stopped at this post. The ones before it are scheduled.",
		"form_published_now":     "The instance published the post instead of scheduling it. Check your profile.",
		"notice_duplicate_media": "Files aren't copied: attach them again.", "files_again": "If you see an error, attach files again.",
		"error_title": "Something went wrong", "error_expired": "This page expired. Try again.", "error_denied": "Access was not granted.",
		"error_instance": "The instance didn't answer as expected.", "error_unavailable": "Temporarily unavailable.", "error_not_found": "Post not found. It may have been published or canceled.",
		"error_too_large": "The files are too large.", "error_bad_form": "The form could not be read.", "error_busy": "Busy with other uploads. Try again in a moment.", "home": "Back",
		"nav_delegations": "Delegations", "post_as": "Post as", "switch": "Switch", "acting_as": "You are acting as @%s. Posts go out from that account.",
		"delegations_title": "Delegations", "delegations_lede": "Let another account of this instance schedule posts as @%s, or see the accounts you can post as.",
		"granted_title": "Who can post as you", "granted_empty": "Nobody.", "revoke": "Revoke",
		"delegate_to": "Account", "delegate_placeholder": "username", "delegate_button": "Authorize",
		"delegate_note":  "The instance will ask for your password, to confirm it is you. The account can then schedule, edit and cancel scheduled posts as you, but not publish right away, change your profile or pass access on. You can revoke it at any time. Posts already scheduled stay in your queue.",
		"received_title": "Accounts you can post as", "received_empty": "None.", "leave": "Give up access",
		"delegations_privacy": "Each authorization keeps a token of the owner's account here, encrypted with its own key, until it is revoked. It also shows under Authorized apps on the instance, and removing it there ends every delegation of that account. Every action is logged with who did it.",
		"flash_delegated":     "Authorized.", "flash_removed": "Access removed.", "flash_removed_unconfirmed": "Removed here, but the instance didn't confirm it. Remove the delegation app under Authorized apps on the instance.",
		"flash_delegation_lost": "A delegation is no longer valid, so you are back on your own account.",
		"delegation_remote":     "Use an account of this instance.", "delegation_no_account": "No active account with that name on this instance.", "delegation_self": "That is your own account.",
		"delegation_too_many": "You reached the limit of delegations.", "delegation_wrong_account": "You authorized with another account. Sign in on the instance as the account you are delegating and try again.",
		"error_not_delegated": "You can't post as that account.", "error_account_changed": "The account changed in another tab. Reload the page and check who you are posting as.",
		"nav_audit": "Log", "audit_title": "Activity log",
		"audit_scope":   "Everything done here on your account, by you or by someone you delegated to, and what you did on other accounts.",
		"audit_limit":   "Only actions made in this app show up. Posts scheduled or canceled directly on Mastodon or in another app don't. Excerpts keep up to 80 characters.",
		"audit_account": "Account", "audit_action": "Action", "audit_all": "All", "audit_from": "From", "audit_to": "To", "audit_apply": "Filter",
		"audit_filter_schedule": "Scheduled", "audit_filter_reschedule": "Date changed", "audit_filter_cancel": "Canceled", "audit_filter_edit": "Edited", "audit_filter_delegation": "Delegation",
		"audit_empty": "Nothing recorded yet.", "audit_older": "Older", "audit_newest": "Newest", "audit_no_text": "(no text)",
		"audit_schedule":   "%[1]s scheduled %[2]s on %[3]s for %[4]s",
		"audit_series":     "%[1]s scheduled a series of %[2]d posts %[3]s on %[4]s, from %[5]s to %[6]s",
		"audit_reschedule": "%[1]s moved %[2]s on %[3]s from %[4]s to %[5]s",
		"audit_cancel":     "%[1]s canceled %[2]s on %[3]s, which was set for %[4]s",
		"audit_edit":       "%[1]s edited %[2]s on %[3]s, now set for %[4]s",
		"audit_delegate":   "%[1]s let %[2]s post as it",
		"audit_revoke":     "%[1]s revoked the access of %[2]s",
		"audit_leave":      "%[1]s gave up access to %[2]s",
		"audit_lost":       "The access of %[1]s to %[2]s ended on the instance",
		"audit_failed":     "Failed", "audit_unknown": "Result unknown", "audit_published_now": "Published right away", "audit_in_progress": "In progress",
		"audit_interrupted": "Interrupted, result unknown", "audit_series_partial": "%d scheduled",
		"source": "Source code", "credit": "Based on mastodon-post-scheduler",
		"kicker": "bolha.io · agende", "part_of": "part of", "nav": "Sections", "hero_a": "Schedule your", "hero_b": "posts",
	},
	"pt-BR": {
		"lang": "pt-BR", "nav_notes": "Novidades", "notes_title": "Novidades", "notes_lede": "O que mudou em cada versão.", "language": "Idioma", "title": "Agende",
		"lede":      "Agende seus posts em %s. A instância guarda e publica, então eles saem mesmo se este site estiver fora do ar.",
		"login":     "Entrar com sua conta",
		"privacy":   "Seu token de acesso fica cifrado durante a sessão e é revogado quando você sai ou depois de 8 horas. O post fica na instância. O rascunho não enviado fica sem cifragem neste navegador por até 7 dias, ou até você agendar ou sair. Aqui ficam suas configurações e um registro das suas ações com até 80 caracteres cifrados do texto, mesmo de posts privados.",
		"nav_queue": "Fila", "nav_calendar": "Calendário", "nav_new": "Novo post", "nav_settings": "Configurações", "logout": "Sair",
		"date_format": "02/01/2006 15:04", "day_format": "02/01/2006",
		"weekdays": "Dom,Seg,Ter,Qua,Qui,Sex,Sáb",
		"months":   "Janeiro,Fevereiro,Março,Abril,Maio,Junho,Julho,Agosto,Setembro,Outubro,Novembro,Dezembro",
		"hours":    "horas", "minutes": "minutos",
		"queue_title": "Posts agendados", "queue_empty": "Nada agendado ainda.", "remaining": "vagas restantes de 300",
		"timezone_note": "Horários em", "open": "Abrir", "duplicate": "Duplicar", "cancel": "Cancelar post", "cancel_confirm": "Cancelar não pode ser desfeito.",
		"has_media": "mídia", "local_only": "Só nesta instância", "expires": "Autodestrói", "markdown": "Markdown",
		"cal_month": "Mês", "cal_week": "Semana", "cal_prev": "Anterior", "cal_next": "Próximo", "no_posts": "Sem posts",
		"new_title": "Novo post", "text": "Texto", "cw": "Aviso de conteúdo", "visibility": "Visibilidade",
		"vis_public": "Público", "vis_unlisted": "Público discreto", "vis_private": "Seguidores", "vis_direct": "Pessoas mencionadas",
		"post_language": "Idioma do post", "reply": "Responder a (URL ou id do post, opcional)", "plain": "Enviar como texto puro",
		"attachment": "Arquivo", "alt": "Descrição da imagem", "when": "Data e hora", "timezone": "Fuso horário",
		"expiry": "Autodestruir depois de publicar", "never": "Nunca", "repeat": "Repetir", "once": "Uma vez", "daily": "Todo dia", "weekly": "Toda semana", "monthly": "Todo mês",
		"times": "Quantos posts", "dates_preview": "Datas", "schedule": "Agendar", "preview": "Prévia (aproximada)", "characters": "caracteres restantes",
		"result_title": "Resultado da série", "result_ok": "Agendado", "result_no": "Não agendado", "back_queue": "Voltar para a fila",
		"post_title": "Post agendado", "edit": "Editar", "edit_title": "Editar post", "keep_file": "Manter este arquivo", "flash_edited": "Post atualizado.", "flash_edited_all": "Post e os próximos com o mesmo texto atualizados.", "siblings_found": "%d agendamentos futuros têm o mesmo texto", "scope_one": "Alterar só este", "scope_future": "Alterar este e os %d próximos", "scope_note": "Cada um dos próximos mantém a própria data e os próprios arquivos. Arquivos novos vão só para este.", "form_later_partial": "Este post foi atualizado, mas alguns dos próximos não. Eles estão marcados abaixo e continuam com o texto antigo.", "edit_blocked": "Este post tem enquete ou citação de outro app, então aqui só dá para mudar a data.", "form_edit_failed": "Não foi possível salvar. O post original continua agendado, sem mudanças.", "form_old_not_cancelled": "A nova versão foi agendada, mas não consegui cancelar a original. Confira a fila e cancele, senão as duas saem.", "error_not_editable": "Este post tem enquete ou citação de outro app e não pode ser editado aqui.", "change_date": "Mudar data", "save": "Salvar", "edit_note": "O Mastodon só deixa mudar a data. Para mudar o texto, duplique o post e cancele este.",
		"settings_title": "Configurações", "default_markdown": "Escrever em Markdown por padrão", "default_visibility": "Visibilidade padrão", "default_language": "Idioma padrão dos posts",
		"flash_created": "Post agendado.", "flash_rescheduled": "Data alterada.", "flash_cancelled": "Post cancelado.", "flash_cancelled_many": "Posts cancelados.", "form_none_selected": "Nenhum post selecionado.", "form_cancel_partial": "Parte dos posts foi cancelada, mas a instância parou de responder. O que ainda aparece abaixo não foi cancelado.", "select_all": "Selecionar todos", "cancel_selected": "Cancelar selecionados", "cancel_many_confirm": "Cancelar os posts selecionados? Não dá para desfazer.", "select_post": "Selecionar", "cancel_scope_one": "Cancelar só este", "cancel_scope_future": "Cancelar este e os %d próximos com o mesmo texto", "flash_saved": "Configurações salvas.",
		"form_invalid": "Há campos inválidos.", "form_empty": "Escreva algo ou anexe um arquivo.", "form_too_soon": "Escolha um horário pelo menos 6 minutos à frente.",
		"form_daily_limit": "Esse dia já tem os 25 posts que o Mastodon permite.", "form_total_limit": "Isso passa dos 300 agendamentos que o Mastodon permite.",
		"form_bad_date": "Escolha uma data válida.", "form_bad_series": "Uma série tem de 2 a 24 posts.", "form_bad_reply": "Use o id ou a URL de um post desta instância.",
		"form_bad_media": "Esse tipo de arquivo não é aceito.", "form_too_many_media": "Arquivos demais.", "form_too_many_uploads": "Arquivos demais para esta série: no máximo 25 envios.",
		"form_bad_timezone": "Fuso horário desconhecido. Escolha um da lista ou use um nome como America/Sao_Paulo.", "form_stopped": "Parou neste post. Os anteriores estão agendados.",
		"form_published_now":     "A instância publicou o post em vez de agendar. Confira seu perfil.",
		"notice_duplicate_media": "Arquivos não são copiados: anexe de novo.", "files_again": "Se aparecer erro, anexe os arquivos de novo.",
		"error_title": "Algo deu errado", "error_expired": "Esta página expirou. Tente de novo.", "error_denied": "O acesso não foi autorizado.",
		"error_instance": "A instância não respondeu como esperado.", "error_unavailable": "Indisponível no momento.", "error_not_found": "Post não encontrado. Ele pode ter sido publicado ou cancelado.",
		"error_too_large": "Os arquivos são grandes demais.", "error_bad_form": "Não foi possível ler o formulário.", "error_busy": "Ocupado com outros envios. Tente de novo em instantes.", "home": "Voltar",
		"nav_delegations": "Delegações", "post_as": "Postar como", "switch": "Trocar", "acting_as": "Você está agindo como @%s. Os posts saem dessa conta.",
		"delegations_title": "Delegações", "delegations_lede": "Permita que outra conta desta instância agende posts como @%s, ou veja as contas em nome das quais você pode postar.",
		"granted_title": "Quem pode postar por você", "granted_empty": "Ninguém.", "revoke": "Revogar",
		"delegate_to": "Conta", "delegate_placeholder": "usuario", "delegate_button": "Autorizar",
		"delegate_note":  "A instância vai pedir sua senha, para confirmar que é você. A conta poderá agendar, editar e cancelar agendamentos em seu nome, mas não publicar na hora, mudar seu perfil ou repassar o acesso. Você pode revogar quando quiser. Os posts já agendados continuam na sua fila.",
		"received_title": "Contas em nome das quais você pode postar", "received_empty": "Nenhuma.", "leave": "Abrir mão do acesso",
		"delegations_privacy": "Cada autorização guarda aqui um token da conta dona, cifrado com chave própria, até ser revogada. Ela também aparece em Apps autorizados na instância, e removê-la lá encerra todas as delegações daquela conta. Toda ação fica registrada com quem fez.",
		"flash_delegated":     "Autorizado.", "flash_removed": "Acesso removido.", "flash_removed_unconfirmed": "Removido aqui, mas a instância não confirmou. Remova o app de delegação em Apps autorizados na instância.",
		"flash_delegation_lost": "Uma delegação não vale mais, então você voltou para a sua conta.",
		"delegation_remote":     "Use uma conta desta instância.", "delegation_no_account": "Não há conta ativa com esse nome nesta instância.", "delegation_self": "Essa é a sua conta.",
		"delegation_too_many": "Você chegou ao limite de delegações.", "delegation_wrong_account": "Você autorizou com outra conta. Entre na instância com a conta que está delegando e tente de novo.",
		"error_not_delegated": "Você não pode postar por essa conta.", "error_account_changed": "A conta mudou em outra aba. Recarregue a página e confira por qual conta você está postando.",
		"nav_audit": "Registro", "audit_title": "Registro de atividades",
		"audit_scope":   "Tudo o que foi feito aqui na sua conta, por você ou por quem recebeu delegação, e o que você fez em outras contas.",
		"audit_limit":   "Só aparecem ações feitas neste app. Posts agendados ou cancelados direto no Mastodon ou em outro app ficam de fora. Os trechos guardam até 80 caracteres.",
		"audit_account": "Conta", "audit_action": "Ação", "audit_all": "Todas", "audit_from": "De", "audit_to": "Até", "audit_apply": "Filtrar",
		"audit_filter_schedule": "Agendou", "audit_filter_reschedule": "Mudou a data", "audit_filter_cancel": "Cancelou", "audit_filter_edit": "Editou", "audit_filter_delegation": "Delegação",
		"audit_empty": "Nada registrado ainda.", "audit_older": "Mais antigos", "audit_newest": "Mais recentes", "audit_no_text": "(sem texto)",
		"audit_schedule":   "%[1]s agendou %[2]s na conta %[3]s para %[4]s",
		"audit_series":     "%[1]s agendou uma série de %[2]d posts %[3]s na conta %[4]s, de %[5]s a %[6]s",
		"audit_reschedule": "%[1]s mudou a data de %[2]s na conta %[3]s de %[4]s para %[5]s",
		"audit_cancel":     "%[1]s cancelou %[2]s na conta %[3]s, que estava para %[4]s",
		"audit_edit":       "%[1]s editou %[2]s na conta %[3]s, agora para %[4]s",
		"audit_delegate":   "%[1]s permitiu que %[2]s postasse por ela",
		"audit_revoke":     "%[1]s revogou o acesso de %[2]s",
		"audit_leave":      "%[1]s abriu mão do acesso a %[2]s",
		"audit_lost":       "O acesso de %[1]s a %[2]s acabou na instância",
		"audit_failed":     "Falhou", "audit_unknown": "Resultado desconhecido", "audit_published_now": "Publicado na hora", "audit_in_progress": "Em andamento",
		"audit_interrupted": "Interrompido, resultado desconhecido", "audit_series_partial": "%d agendados",
		"source": "Código-fonte", "credit": "Baseado no mastodon-post-scheduler",
		"kicker": "bolha.io · agende", "part_of": "parte da", "nav": "Seções", "hero_a": "Agende seus", "hero_b": "posts",
	},
}

func strs(lang string) map[string]string {
	if t, ok := translations[lang]; ok {
		return t
	}
	return translations["en"]
}

func normalize(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if strings.HasPrefix(lang, "pt") {
		return "pt-BR"
	}
	if strings.HasPrefix(lang, "en") {
		return "en"
	}
	return ""
}

// language picks the cookie, then the browser's languages, then the configured default.
func language(r *http.Request, fallback string) string {
	if q := normalize(r.URL.Query().Get("lang")); q != "" {
		return q
	}
	if c, err := r.Cookie(langCookie); err == nil {
		if l := normalize(c.Value); l != "" {
			return l
		}
	}
	for part := range strings.SplitSeq(r.Header.Get("Accept-Language"), ",") {
		if l := normalize(strings.SplitN(part, ";", 2)[0]); l != "" {
			return l
		}
	}
	if l := normalize(fallback); l != "" {
		return l
	}
	return "en"
}

func setLanguage(w http.ResponseWriter, value string) {
	if l := normalize(value); l != "" && slices.Contains([]string{"en", "pt-BR"}, l) {
		http.SetCookie(w, &http.Cookie{Name: langCookie, Value: l, Path: "/", MaxAge: 31536000, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	}
}
