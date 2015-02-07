/**
 * Created by Dio on 7/2/2015.
 */
var mongoose = require('mongoose');
var Schema = mongoose.Schema;
var TodoSchema = new Schema({
	name: String,
	completed: Boolean,
	note: String,
	updated_at: { type: Date, default: Date.now }
});

module.exports = mongoose.model('Todo', TodoSchema);