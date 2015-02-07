/**
 * Created by Dio on 7/2/2015.
 */
'use strict';
var express = require('express');
var router = express.Router();
var mongoose = require('mongoose');
var ToDo = mongoose.model('Todo');
//endregion

//TODO: move to controllers
router.post('/todo/create', function (req, res) {
	new ToDo({
		content:   req.body.content,
		update_at: Date.now()
	}).save(function (err, todo, count) {
			if (err) {
				console.log('err:' + err);
			}
			res.redirect('/');
		});
});

//TODO: move to controllers
router.get('/todo/list', function (req, res) {
	var user_id = req.cookies ? req.cookies.user_id : undefined;

	Todo
		.find({user_id: user_id})
		.exec(function (err, todos,count) {
			res.render('todo/list', {
				title: 'Todo list',
				todos: todos,
				totalCount: count
			});
		});
});

//TODO: move to controllers
router.get('/todo/:id', function (req, res, err, id) {
	//render view
	res.render('/todo/detail', {item: Todo.find({ObjectId: id})});
});

module.exports = router;