package main

import (
	"net/http"
	"slices"
	"strings"
)

const langCookie = "__Host-agende-lang"

var translations = map[string]map[string]string{
	"en": {
		"lang": "en", "language": "Language", "title": "Agende",
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
		"expiry": "Self-destruct after publishing", "never": "Never", "repeat": "Repeat", "once": "Once", "weekly": "Every week", "monthly": "Every month",
		"times": "How many posts", "dates_preview": "Dates", "schedule": "Schedule", "preview": "Preview (approximate)", "characters": "characters left",
		"result_title": "Series result", "result_ok": "Scheduled", "result_no": "Not scheduled", "back_queue": "Back to the queue",
		"post_title": "Scheduled post", "change_date": "Change date", "save": "Save", "edit_note": "Mastodon only lets the date change. To change the text, duplicate the post and cancel this one.",
		"settings_title": "Settings", "default_markdown": "Write in Markdown by default", "default_visibility": "Default visibility", "default_language": "Default post language",
		"flash_created": "Post scheduled.", "flash_rescheduled": "Date changed.", "flash_cancelled": "Post canceled.", "flash_saved": "Settings saved.",
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
		"source": "Source code", "credit": "Based on mastodon-post-scheduler",
	},
	"pt-BR": {
		"lang": "pt-BR", "language": "Idioma", "title": "Agende",
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
		"expiry": "Autodestruir depois de publicar", "never": "Nunca", "repeat": "Repetir", "once": "Uma vez", "weekly": "Toda semana", "monthly": "Todo mês",
		"times": "Quantos posts", "dates_preview": "Datas", "schedule": "Agendar", "preview": "Prévia (aproximada)", "characters": "caracteres restantes",
		"result_title": "Resultado da série", "result_ok": "Agendado", "result_no": "Não agendado", "back_queue": "Voltar para a fila",
		"post_title": "Post agendado", "change_date": "Mudar data", "save": "Salvar", "edit_note": "O Mastodon só deixa mudar a data. Para mudar o texto, duplique o post e cancele este.",
		"settings_title": "Configurações", "default_markdown": "Escrever em Markdown por padrão", "default_visibility": "Visibilidade padrão", "default_language": "Idioma padrão dos posts",
		"flash_created": "Post agendado.", "flash_rescheduled": "Data alterada.", "flash_cancelled": "Post cancelado.", "flash_saved": "Configurações salvas.",
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
		"source": "Código-fonte", "credit": "Baseado no mastodon-post-scheduler",
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
