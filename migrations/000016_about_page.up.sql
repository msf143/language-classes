-- The About page (decided 2026-11-01): plain text in both languages, which
-- anyone may read and admins may edit. A line starting with "#" shows as a
-- heading; the page never treats the text as HTML.
--
-- Every save adds a row and the newest row is the page, like a journal: an
-- accidental wipe can be recovered by hand. So there's no updated_at; rows are
-- never changed. saved_by is NULL only for this first text.
CREATE TABLE about_page_version (
    id                  uuid PRIMARY KEY,
    body_en             text NOT NULL CHECK (length(btrim(body_en)) > 0),
    body_es             text NOT NULL CHECK (length(btrim(body_es)) > 0),
    saved_by            uuid REFERENCES account (id),
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX about_page_version_newest_idx ON about_page_version (created_at DESC);

INSERT INTO about_page_version (id, body_en, body_es) VALUES ('01900000-0000-7000-8000-00000000ab01',
$about$# Real People. Real Conversations. Real Language Learning.

At Hablemos.live, we believe language is meant to be spoken, not just studied. That's why our platform focuses on live, interactive conversations that help students develop practical communication skills from day one.

Founded by two brothers, one from the United States and one from Colombia, Hablemos.live was created to connect language learners with experienced teachers in an engaging and affordable environment.

Choose a teacher, schedule a session, and start speaking. As additional students join a class, the cost per student decreases, making quality language instruction more accessible for everyone.

No endless drills. No boring worksheets. Just meaningful conversations that help you gain confidence and fluency.

Hablemos. Let's talk.$about$,
$about$# Personas reales. Conversaciones reales. Aprendizaje real de idiomas.

En Hablemos.live creemos que un idioma está hecho para hablarse, no solo para estudiarse. Por eso nuestra plataforma se centra en conversaciones en vivo e interactivas que ayudan a los estudiantes a desarrollar habilidades prácticas de comunicación desde el primer día.

Fundada por dos hermanos, uno de Estados Unidos y otro de Colombia, Hablemos.live nació para conectar a quienes aprenden idiomas con profesores experimentados en un ambiente ameno y económico.

Elige un profesor, agenda una clase y empieza a hablar. A medida que más estudiantes se unen a una clase, el costo por estudiante baja, lo que hace que la enseñanza de idiomas de calidad sea más accesible para todos.

Sin ejercicios interminables. Sin hojas de trabajo aburridas. Solo conversaciones con sentido que te ayudan a ganar confianza y fluidez.

Hablemos. Let's talk.$about$);
