/**
 * Created by Dio on 7/2/2015.
 */
'use strict';
var utils = require('../utils');
var express = require('express');
var router = express.Router();
var mongoose = require('mongoose');
var toDoSchema = mongoose.model('Todo');
//endregion
router.get('/init', function (req, res) {
	new toDoSchema({
		user_id:   uid,
		name:      "auto notes " + new Date().toUTCString(),
		completed: false,
		update_at: Date.now()
	})
		.save(function (err, todo) {
			if (err) {
				return next(err);
			}
			res.redirect('/todo/list');
		});
});

router.get('/create', function (req, res) {
	res.render('todo/create');
});

router.post('/create', function (req, res) {
	new toDoSchema({
		name:      req.body.name,
		note:      req.body.note,
		completed: false,
		update_at: Date.now()
	}).save(function (err, todo, count) {
			if (err) {
				console.log('err:' + err);
			}
			res.redirect('/todo/list');
		});
});
router.get('/list', function (req, res) {
	toDoSchema
		.find()
		.limit(20)
		.sort('update_at')
		.exec(function (err, foundDocs) {
			res.render('todo/list', {
				title: String.format('listing 20 docs'),
				count: foundDocs.length,
				items: foundDocs
			});
		});
});
router.get('/list/:q', function (req, res) {
	var q = req.params.q ? req.params.q : /.*/;

	toDoSchema
		.find({note: /q/i})
		.limit(10)
		.sort('update_at')
		.exec(function (err, foundDocs) {
			res.render('todo/list', {
				title: String.format('Search result for {note} contains "{0}"', q),
				count: foundDocs.length,
				items: foundDocs
			});
		});
});

router.get('/delete/:id', function (req, res, id) {
	console.log('deleting item id:' + req.params.id);
	toDoSchema.findById(req.params.id, function (err, doc) {
		if (err) {
			console.log(err);
		}
		doc.remove();
	});
	res.redirect('/todo/list');
});

router.get('/update/:id', function (req, res, id) {
	console.log('Searching for id=' + req.params.id);
	toDoSchema.findById(req.params.id, function (err, doc) {
		if (err) {
			console.log(err);
		}
		res.render('todo/update', {item: doc});
	});
});

router.post('/update', function (req, res, next) {
	console.log('updating item: ' + req.body.id);

	toDoSchema.findById(req.body.id, function (err, found) {
		found.note = req.body.note;
		found.name = req.body.name;
		found.update_at = Date.now();
		found.save();
	});

	res.redirect('/todo/list');
})
module.exports = router;