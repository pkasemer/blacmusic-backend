-- One profile picture per person: the user's avatar is also the artist's image.
-- artists.avatar_image_id is no longer read or written (kept for now so this
-- migration cannot lose data); artists with an owner show the owner's avatar.
ALTER TABLE users ADD COLUMN avatar_image_id text;

-- Carry over avatars artists already had.
UPDATE users u
SET avatar_image_id = a.avatar_image_id
FROM artists a
WHERE a.owner_user_id = u.id AND a.avatar_image_id IS NOT NULL AND u.avatar_image_id IS NULL;
UPDATE artists SET avatar_image_id = NULL WHERE owner_user_id IS NOT NULL;
