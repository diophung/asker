/**
 * Created by Dio on 4/2/2015.
 */
var mongoose = require( 'mongoose' );
var Schema = mongoose.Schema;

var TodoSchema = new Schema({
	user_id: String,
	name: String,
	completed: Boolean,
	note: String,
	updated_at: { type: Date, default: Date.now }
});

mongoose.model('Todo', TodoSchema);
mongoose.connect( 'mongodb://localhost/asker' );