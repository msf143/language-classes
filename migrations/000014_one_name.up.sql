-- One name per person (decided 2026-10-25): the account holder's own learner
-- row always carries the account's name. Before this, editing your own row in
-- My Account renamed only the row, and older renames matched by name, so some
-- accounts showed one name in the header and another on "(you)". The account
-- name wins: it's what "Your name" shows and what people set last. From now on
-- the app keeps the two equal (PUT /me and PUT /me/learners/{id} write both).
UPDATE learner l SET display_name = a.display_name
FROM account a
WHERE a.id = l.account_id AND l.is_account_holder AND l.display_name <> a.display_name;
