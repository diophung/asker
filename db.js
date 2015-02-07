/**
 * Created by Dio on 4/2/2015.
 */
var mongoose = require( 'mongoose' );
mongoose.connect( 'mongodb://localhost/asker' );

var db = mongoose.connection;
db.on('error', console.error.bind(console, 'connection error:'));
db.once('open', function (callback) {
	// yay!
	console.log('db connection opened');
});