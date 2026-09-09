CREATE INDEX messages_unread ON messages(room,id,sender_user_id) WHERE service = 0;
