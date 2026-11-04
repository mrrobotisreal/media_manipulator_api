package assistant

import (
	"fmt"
	"strings"
)

// The guide's answer when the spend cap is reached (decision 8): no model call,
// the top 3 retrieved docs as links, framed by a short localized sentence.
// English from MM104; the other 17 site locales from MM107 (model-written,
// native review pending like the rest of the non-English copy).

// staticFrames maps a locale prefix (lowercase language, or language-region)
// to {intro, none}. intro introduces the links; none is used when retrieval
// found nothing.
var staticFrames = map[string][2]string{
	"en": {
		"I'm taking a short break today, so here are the help pages that match your question:",
		"I'm taking a short break today. You can search the help pages or browse all tools at /tools.",
	},
	"uk": {
		"Сьогодні я трохи відпочиваю, тож ось сторінки довідки, які відповідають вашому запитанню:",
		"Сьогодні я трохи відпочиваю. Пошукайте на сторінках довідки або перегляньте всі інструменти на /tools.",
	},
	"ru": {
		"Сегодня я немного отдыхаю, поэтому вот страницы справки, которые подходят к вашему вопросу:",
		"Сегодня я немного отдыхаю. Поищите на страницах справки или посмотрите все инструменты на /tools.",
	},
	"he": {
		"היום אני לוקח הפסקה קצרה, אז הנה דפי העזרה שמתאימים לשאלה שלך:",
		"היום אני לוקח הפסקה קצרה. אפשר לחפש בדפי העזרה או לעיין בכל הכלים בכתובת /tools.",
	},
	"de": {
		"Ich mache heute eine kurze Pause, deshalb hier die Hilfeseiten, die zu deiner Frage passen:",
		"Ich mache heute eine kurze Pause. Du kannst die Hilfeseiten durchsuchen oder alle Tools unter /tools ansehen.",
	},
	"es": {
		"Hoy me estoy tomando un pequeño descanso, así que aquí tienes las páginas de ayuda que encajan con tu pregunta:",
		"Hoy me estoy tomando un pequeño descanso. Puedes buscar en las páginas de ayuda o ver todas las herramientas en /tools.",
	},
	"es-mx": {
		"Hoy me estoy tomando un pequeño descanso, así que aquí están las páginas de ayuda que corresponden a tu pregunta:",
		"Hoy me estoy tomando un pequeño descanso. Puedes buscar en las páginas de ayuda o ver todas las herramientas en /tools.",
	},
	"fr": {
		"Je fais une petite pause aujourd’hui, alors voici les pages d’aide qui correspondent à votre question :",
		"Je fais une petite pause aujourd’hui. Vous pouvez chercher dans les pages d’aide ou parcourir tous les outils sur /tools.",
	},
	"it": {
		"Oggi mi prendo una breve pausa, quindi ecco le pagine di aiuto che corrispondono alla tua domanda:",
		"Oggi mi prendo una breve pausa. Puoi cercare nelle pagine di aiuto o sfogliare tutti gli strumenti su /tools.",
	},
	"ja": {
		"今日は少しお休みしているため、ご質問に合うヘルプページをご案内します：",
		"今日は少しお休みしています。ヘルプページを検索するか、/tools ですべてのツールをご覧ください。",
	},
	"zh-cn": {
		"我今天稍作休息，以下是与你的问题相关的帮助页面：",
		"我今天稍作休息。你可以搜索帮助页面，或在 /tools 浏览所有工具。",
	},
	"zh-tw": {
		"我今天稍作休息，以下是與你的問題相關的說明頁面：",
		"我今天稍作休息。你可以搜尋說明頁面，或在 /tools 瀏覽所有工具。",
	},
	"ko": {
		"오늘은 잠시 쉬고 있어요. 질문과 관련된 도움말 페이지를 알려 드릴게요:",
		"오늘은 잠시 쉬고 있어요. 도움말 페이지를 검색하거나 /tools에서 모든 도구를 둘러보세요.",
	},
	"ar": {
		"أنا في استراحة قصيرة اليوم، لذا إليك صفحات المساعدة التي تتعلق بسؤالك:",
		"أنا في استراحة قصيرة اليوم. يمكنك البحث في صفحات المساعدة أو تصفح جميع الأدوات على ‎/tools.",
	},
	"hi": {
		"आज मैं थोड़ा आराम कर रहा हूँ, इसलिए आपके सवाल से जुड़े सहायता पेज ये रहे:",
		"आज मैं थोड़ा आराम कर रहा हूँ। आप सहायता पेज खोज सकते हैं या /tools पर सभी टूल देख सकते हैं।",
	},
	"pt": {
		"Hoje estou a fazer uma pequena pausa, por isso aqui estão as páginas de ajuda que correspondem à sua pergunta:",
		"Hoje estou a fazer uma pequena pausa. Pode pesquisar nas páginas de ajuda ou ver todas as ferramentas em /tools.",
	},
	"pt-br": {
		"Hoje estou fazendo uma pequena pausa, então aqui estão as páginas de ajuda que combinam com a sua pergunta:",
		"Hoje estou fazendo uma pequena pausa. Você pode pesquisar nas páginas de ajuda ou ver todas as ferramentas em /tools.",
	},
	"vi": {
		"Hôm nay mình đang nghỉ ngơi một chút, nên đây là các trang trợ giúp phù hợp với câu hỏi của bạn:",
		"Hôm nay mình đang nghỉ ngơi một chút. Bạn có thể tìm trong các trang trợ giúp hoặc xem tất cả công cụ tại /tools.",
	},
	// Aliases: the region tags whose language prefix would pick another variant.
	"es-es": {
		"Hoy me estoy tomando un pequeño descanso, así que aquí tienes las páginas de ayuda que encajan con tu pregunta:",
		"Hoy me estoy tomando un pequeño descanso. Puedes buscar en las páginas de ayuda o ver todas las herramientas en /tools.",
	},
	"pt-pt": {
		"Hoje estou a fazer uma pequena pausa, por isso aqui estão as páginas de ajuda que correspondem à sua pergunta:",
		"Hoje estou a fazer uma pequena pausa. Pode pesquisar nas páginas de ajuda ou ver todas as ferramentas em /tools.",
	},
	"zh": {
		"我今天稍作休息，以下是与你的问题相关的帮助页面：",
		"我今天稍作休息。你可以搜索帮助页面，或在 /tools 浏览所有工具。",
	},
	"zh-hk": {
		"我今天稍作休息，以下是與你的問題相關的說明頁面：",
		"我今天稍作休息。你可以搜尋說明頁面，或在 /tools 瀏覽所有工具。",
	},
}

// StaticFrame returns the frame pair for a locale (falls back to English).
func StaticFrame(locale string) (intro, none string) {
	l := strings.ToLower(strings.TrimSpace(locale))
	if f, ok := staticFrames[l]; ok {
		return f[0], f[1]
	}
	if i := strings.IndexAny(l, "-_"); i > 0 {
		if f, ok := staticFrames[l[:i]]; ok {
			return f[0], f[1]
		}
	}
	f := staticFrames["en"]
	return f[0], f[1]
}

// StaticAnswer renders the retrieval-only answer as markdown.
func StaticAnswer(locale string, hits []Hit) string {
	intro, none := StaticFrame(locale)
	var links []string
	for _, h := range hits {
		if h.Doc.URL == "" {
			continue
		}
		links = append(links, fmt.Sprintf("- [%s](%s)", h.Doc.Title, h.Doc.URL))
		if len(links) == 3 {
			break
		}
	}
	if len(links) == 0 {
		return none
	}
	return intro + "\n\n" + strings.Join(links, "\n")
}

// outOfScopeLines is the guide's reply to a clearly off-topic message (MM105
// decision 7), keyed like staticFrames: all 18 site locales (MM107).
var outOfScopeLines = map[string]string{
	"en":    "I'm Medi, the Media Manipulator guide, so I stick to the site, its tools and media editing (video, photos, audio, animation, formats). Ask me which tool to use or how to edit something and I'll help.",
	"uk":    "Я Medi, гід Media Manipulator, тому говорю лише про сайт, його інструменти та редагування медіа (відео, фото, аудіо, анімація, формати). Запитайте, який інструмент обрати або як щось відредагувати, і я допоможу.",
	"ru":    "Я Medi, гид Media Manipulator, поэтому говорю только о сайте, его инструментах и редактировании медиа (видео, фото, аудио, анимация, форматы). Спросите, какой инструмент выбрать или как что-то отредактировать, и я помогу.",
	"he":    "אני Medi, המדריך של Media Manipulator, ולכן אני עונה רק על האתר, הכלים שלו ועריכת מדיה (וידאו, תמונות, אודיו, אנימציה, פורמטים). שאלו אותי באיזה כלי להשתמש או איך לערוך משהו, ואשמח לעזור.",
	"de":    "Ich bin Medi, der Guide von Media Manipulator, und bleibe deshalb bei der Website, ihren Tools und der Medienbearbeitung (Video, Fotos, Audio, Animation, Formate). Frag mich, welches Tool du brauchst oder wie du etwas bearbeitest, dann helfe ich dir.",
	"es":    "Soy Medi, la guía de Media Manipulator, así que me centro en el sitio, sus herramientas y la edición multimedia (vídeo, fotos, audio, animación, formatos). Pregúntame qué herramienta usar o cómo editar algo y te ayudo.",
	"es-mx": "Soy Medi, la guía de Media Manipulator, así que me enfoco en el sitio, sus herramientas y la edición multimedia (video, fotos, audio, animación, formatos). Pregúntame qué herramienta usar o cómo editar algo y te ayudo.",
	"fr":    "Je suis Medi, le guide de Media Manipulator : je m’en tiens au site, à ses outils et à l’édition de médias (vidéo, photos, audio, animation, formats). Demandez-moi quel outil utiliser ou comment modifier quelque chose, et je vous aiderai.",
	"it":    "Sono Medi, la guida di Media Manipulator, quindi mi occupo solo del sito, dei suoi strumenti e del montaggio dei media (video, foto, audio, animazione, formati). Chiedimi quale strumento usare o come modificare qualcosa e ti aiuto.",
	"ja":    "私は Media Manipulator のガイド、Medi です。サイトとツール、メディア編集（動画、写真、音声、アニメーション、形式）についてお手伝いしています。どのツールを使えばよいか、どう編集すればよいかを聞いてください。",
	"zh-cn": "我是 Medi，Media Manipulator 的向导，只回答与本站、它的工具和媒体编辑（视频、照片、音频、动画、格式）有关的问题。问我该用哪个工具或怎样编辑，我来帮你。",
	"zh-tw": "我是 Medi，Media Manipulator 的嚮導，只回答與本站、它的工具和媒體編輯（影片、照片、音訊、動畫、格式）有關的問題。問我該用哪個工具或怎麼編輯，我來幫你。",
	"ko":    "저는 Media Manipulator 가이드 Medi예요. 사이트와 도구, 미디어 편집(동영상, 사진, 오디오, 애니메이션, 형식)에 관한 질문만 도와드려요. 어떤 도구를 써야 할지, 무언가를 어떻게 편집할지 물어보세요.",
	"ar":    "أنا Medi، دليل Media Manipulator، لذا أتحدث فقط عن الموقع وأدواته وتحرير الوسائط (الفيديو والصور والصوت والرسوم المتحركة والصيغ). اسألني عن الأداة المناسبة أو عن طريقة تعديل شيء ما وسأساعدك.",
	"hi":    "मैं Medi हूँ, Media Manipulator का गाइड, इसलिए मैं सिर्फ़ साइट, उसके टूल और मीडिया एडिटिंग (वीडियो, फ़ोटो, ऑडियो, एनिमेशन, फ़ॉर्मैट) की बात करता हूँ। मुझसे पूछिए कि कौन-सा टूल इस्तेमाल करें या कुछ कैसे एडिट करें, मैं मदद करूँगा।",
	"pt":    "Sou o Medi, o guia do Media Manipulator, por isso falo apenas do site, das suas ferramentas e da edição de multimédia (vídeo, fotografias, áudio, animação, formatos). Pergunte-me que ferramenta usar ou como editar algo e eu ajudo.",
	"pt-br": "Sou o Medi, o guia do Media Manipulator, então falo só sobre o site, suas ferramentas e edição de mídia (vídeo, fotos, áudio, animação, formatos). Pergunte qual ferramenta usar ou como editar algo e eu ajudo.",
	"vi":    "Mình là Medi, hướng dẫn viên của Media Manipulator, nên mình chỉ nói về trang web, các công cụ và việc chỉnh sửa media (video, ảnh, âm thanh, hoạt ảnh, định dạng). Hãy hỏi mình nên dùng công cụ nào hoặc chỉnh sửa thế nào, mình sẽ giúp.",
	"es-es": "Soy Medi, la guía de Media Manipulator, así que me centro en el sitio, sus herramientas y la edición multimedia (vídeo, fotos, audio, animación, formatos). Pregúntame qué herramienta usar o cómo editar algo y te ayudo.",
	"pt-pt": "Sou o Medi, o guia do Media Manipulator, por isso falo apenas do site, das suas ferramentas e da edição de multimédia (vídeo, fotografias, áudio, animação, formatos). Pergunte-me que ferramenta usar ou como editar algo e eu ajudo.",
	"zh":    "我是 Medi，Media Manipulator 的向导，只回答与本站、它的工具和媒体编辑（视频、照片、音频、动画、格式）有关的问题。问我该用哪个工具或怎样编辑，我来帮你。",
	"zh-hk": "我是 Medi，Media Manipulator 的嚮導，只回答與本站、它的工具和媒體編輯（影片、照片、音訊、動畫、格式）有關的問題。問我該用哪個工具或怎麼編輯，我來幫你。",
}

// OutOfScopeLine returns the out-of-scope reply for a locale (English fallback).
func OutOfScopeLine(locale string) string {
	l := strings.ToLower(strings.TrimSpace(locale))
	if s, ok := outOfScopeLines[l]; ok {
		return s
	}
	if i := strings.IndexAny(l, "-_"); i > 0 {
		if s, ok := outOfScopeLines[l[:i]]; ok {
			return s
		}
	}
	return outOfScopeLines["en"]
}
