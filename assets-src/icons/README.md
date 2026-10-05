# Иконки для архива Dozor

Все три изображения сгенерированы отдельными вызовами встроенного `image_gen`. В качестве визуальных референсов использовался **атлас AtmosphereIcon из monorepo-frontend packages/00**: Image 1 — полный атлас, Image 2 — строка с dashcam, viewfinder, video tile и автомобилями. Референсы задавали только стиль, толщину контура, опаловую заливку и лёгкую перспективу.

## `storage-drive`

Назначение: локальное дисковое хранилище записей. На иконке один компактный внешний HDD с индикатором состояния и коротким кабелем сбоку.

Референсы: атлас AtmosphereIcon из monorepo-frontend packages/00 — Image 1 (полный атлас) и Image 2 (строка dashcam/viewfinder/video tile/cars).

Точный промпт:

```text
Use case: stylized-concept
Asset type: one 1024x1024 raster sticker icon for the Dozor home video archive web UI.
Primary request: Create exactly one centered compact external hard disk drive icon, named storage-drive: a rounded rectangular box in slight playful perspective, with a small round status light on the front and a short cable stub emerging from one side. It represents local disk storage for recordings.
Input images: Image 1 is a style reference: the full existing AtmosphereIcon atlas, 40 icons on a light neutral background. Image 2 is a style reference: a full-resolution atlas row with a dashcam, viewfinder, video tile, and cars. Use both only to match the illustration system, stroke weight, holographic fill, and playful perspective; do not copy any reference object.
Scene/backdrop: perfectly flat, uniform pure chroma green #00FF00 technical matte filling the entire 1024x1024 canvas, including every hole and open gap; opaque green background, not transparency.
Subject: exactly one compact external hard disk drive, rounded rectangular body in slight perspective, one small circular status light, one short cable stub on one side, no extra components.
Style/medium: flat sticker-like hand-drawn illustration with bold black rounded outlines, slight organic unevenness, continuous black stroke around the outer silhouette and every interior feature, matching the reference atlas at about 9px stroke weight at a 512px cell scale.
Composition/framing: one object centered, its longest side about 70% of the canvas, generous green margins, no cropping.
Lighting/mood: flat graphic treatment; no realistic lighting, no cast shadow.
Color palette: opaque luminous holographic opal foil with irregular flowing marbled bands of lavender, turquoise, mint, pink, peach, and pale yellow.
Materials/textures: smooth flat holographic sticker fill with flowing cloudy marbling; no chrome and no glitter.
Text (verbatim): none.
Constraints: pure green must remain clean and flat around the object; keep black outlines free of green spill; preserve clear green holes/open gaps; output 1024x1024.
Avoid: any second object, multiple drives, extra cables, labels, text, letters, numbers, logos, watermark, white sticker border, cast shadow, photorealistic 3D, chrome, solid black shading, glitter, sparkles, gradients in the background, pale halo.
```

## `cloud-backup`

Назначение: копия записей в облачном S3-хранилище. На иконке одно мягкое облако и одна крупная стрелка загрузки внутри него.

Референсы: атлас AtmosphereIcon из monorepo-frontend packages/00 — Image 1 (полный атлас) и Image 2 (строка dashcam/viewfinder/video tile/cars).

Точный промпт:

```text
Use case: stylized-concept
Asset type: one 1024x1024 raster sticker icon for the Dozor home video archive web UI.
Primary request: Create exactly one centered soft rounded cloud icon named cloud-backup, with a bold upward arrow inside the cloud. The arrow must be a separate holographic filled shape with its own bold black rounded outline, clearly readable as an upward upload/backup arrow. It represents a copy of recordings in cloud storage (S3).
Input images: Image 1 is a style reference: the full existing AtmosphereIcon atlas, 40 icons on a light neutral background. Image 2 is a style reference: a full-resolution atlas row with a dashcam, viewfinder, video tile, and cars. Use both only to match the illustration system, stroke weight, holographic fill, and playful perspective; do not copy any reference object.
Scene/backdrop: perfectly flat, uniform pure chroma green #00FF00 technical matte filling the entire 1024x1024 canvas, including every hole and open gap inside and around the arrow; opaque green background, not transparency.
Subject: exactly one soft rounded cloud silhouette with one bold upward arrow centered inside it; the arrow is black-outlined and holographic-filled, with a simple shaft and triangular arrowhead, no extra symbols.
Style/medium: flat sticker-like hand-drawn illustration with bold black rounded outlines, slight organic unevenness, continuous black stroke around the outer cloud silhouette and around every arrow feature, matching the reference atlas at about 9px stroke weight at a 512px cell scale.
Composition/framing: one cloud-and-arrow icon centered, its longest side about 70% of the canvas, generous green margins, no cropping, arrow legible at icon size.
Lighting/mood: flat graphic treatment; no realistic lighting, no cast shadow.
Color palette: opaque luminous holographic opal foil with irregular flowing marbled bands of lavender, turquoise, mint, pink, peach, and pale yellow.
Materials/textures: smooth flat holographic sticker fill with flowing cloudy marbling on both cloud and arrow; no chrome and no glitter.
Text (verbatim): none.
Constraints: pure green must remain clean and flat around and inside all open gaps; keep black outlines free of green spill; preserve the green negative space between arrow and cloud where present; output 1024x1024.
Avoid: any second object, rain, sun, drops, extra arrows, icons, text, letters, numbers, logos, watermark, white sticker border, cast shadow, photorealistic 3D, chrome, solid black shading, glitter, sparkles, gradients in the background, pale halo.
```

## `system-update`

Назначение: автоматические обновления ПО. На иконке одна небольшая плата/микросхема и одно круговое обновление с двумя стрелочными наконечниками.

Референсы: атлас AtmosphereIcon из monorepo-frontend packages/00 — Image 1 (полный атлас) и Image 2 (строка dashcam/viewfinder/video tile/cars).

Точный промпт:

```text
Use case: stylized-concept
Asset type: one 1024x1024 raster sticker icon for the Dozor home video archive web UI.
Primary request: Create exactly one centered system-update icon: a small rounded device board or chip with a circular refresh arrow curling around it. The refresh ring must have two clear arrowheads chasing each other around the circle, both arrowheads drawn as black-outlined holographic shapes. It represents automatic software updates.
Input images: Image 1 is a style reference: the full existing AtmosphereIcon atlas, 40 icons on a light neutral background. Image 2 is a style reference: a full-resolution atlas row with a dashcam, viewfinder, video tile, and cars. Use both only to match the illustration system, stroke weight, holographic fill, and playful perspective; do not copy any reference object.
Scene/backdrop: perfectly flat, uniform pure chroma green #00FF00 technical matte filling the entire 1024x1024 canvas, including every hole and open gap inside the circular arrow; opaque green background, not transparency.
Subject: exactly one compact rounded rectangular device board/chip in a slight playful perspective, encircled by one circular refresh arrow made from a rounded black-outlined holographic band with two distinct arrowheads pointing along the circle in the same direction; no other components or symbols.
Style/medium: flat sticker-like hand-drawn illustration with bold black rounded outlines, slight organic unevenness, continuous black stroke around the board silhouette, circular arrow band, both arrowheads, and every interior feature, matching the reference atlas at about 9px stroke weight at a 512px cell scale.
Composition/framing: one combined board-and-refresh icon centered, its longest side about 70% of the canvas, generous green margins, no cropping, two arrowheads clearly visible.
Lighting/mood: flat graphic treatment; no realistic lighting, no cast shadow.
Color palette: opaque luminous holographic opal foil with irregular flowing marbled bands of lavender, turquoise, mint, pink, peach, and pale yellow.
Materials/textures: smooth flat holographic sticker fill with flowing cloudy marbling on board and refresh arrow; no chrome and no glitter.
Text (verbatim): none.
Constraints: pure green must remain clean and flat around the object and inside the refresh loop; keep black outlines free of green spill; preserve the green negative-space hole inside the circular arrow; output 1024x1024.
Avoid: extra chips, extra arrows beyond the two arrowheads on one ring, circuit labels, text, letters, numbers, logos, watermark, white sticker border, cast shadow, photorealistic 3D, chrome, solid black shading, glitter, sparkles, gradients in the background, pale halo.
```

## Удаление фона и контроль краёв

1. Raw-файлы приведены к 1024×1024 через Pillow (`RGB`, `LANCZOS`). Для выполнения framing-требования объект был вырезан по alpha-bbox исходного результата, масштабирован так, чтобы его длинная сторона была 717 px (`round(1024 × 0.70)`), и помещён по центру на новый matte. Получились размеры объекта: `storage-drive` — 717×402 px, `cloud-backup` — 717×516 px, `system-update` — 717×714 px. Пиксели технической зелёной маски с условиями `g >= 220`, `g - r >= 170`, `g - b >= 170` нормализованы в точный `#00FF00`; это исправляет небольшое цветовое отклонение, которое дала генерация.
2. Фон удалён helper-скриптом `remove_chroma_key.py` из навыка `imagegen` с одинаковыми параметрами для всех файлов:

   ```text
   --key-color #00ff00
   --tolerance 32
   --soft-matte: выключен
   --edge-contract 0
   --edge-feather 0
   --spill-cleanup: выключен
   ```

   То есть использована строгая hard-key маска без feathering, чтобы не получить полупрозрачный зелёный или белый halo.
3. После helper выполнен компонентный despill: построены 8-связные компоненты непрозрачных пикселей с условиями `g - r >= 10` и `g - b >= 10`; только компоненты, соприкасающиеся с прозрачностью, заменены на чистый `(0, 0, 0)`. Это убирает зелёный остаток в антиалиасинговой кромке и не меняет внутренние цветные опаловые области. У всех пикселей с `alpha == 0` RGB принудительно обнулён.

Итог: raw-файлы — RGB 1024×1024 с зелёной matte-заливкой; финальные файлы — RGBA 1024×1024 с прозрачными полями и без частично прозрачных пикселей. Края проверены на белом фоне: обводка непрерывная, зелёной кромки и светлого halo не обнаружено.
